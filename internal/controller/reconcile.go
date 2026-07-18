package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// reconcileFleet drives one fleet toward its desired replica count.
// The work queue guarantees a fleet is never reconciled
// by two workers at once, so counting and acting here is race-free within
// the controller.
func (c *Controller) reconcileFleet(ctx context.Context, fleetID string) error {
	defer func(start time.Time) {
		c.metrics.ReconcileDuration(fleetID, time.Since(start))
	}(time.Now())

	fleet, err := c.store.GetFleet(ctx, fleetID)
	if errors.Is(err, store.ErrNotFound) {
		// Deleted. The API refuses deletion while servers are live, so
		// there is nothing to tear down.
		return nil
	}
	if err != nil {
		return err
	}
	gss, err := c.store.ListAllGameServersByFleet(ctx, fleetID, "")
	if err != nil {
		return err
	}

	// Health sweep (app layer): bulk-check heartbeats for
	// Ready/Allocated/Reserved servers. nil means Redis was unreachable (or
	// health.disabled) — never write servers off then. Task
	// layer detection (STOPPED events) stays on either way.
	var alive map[string]bool
	if !c.healthDisabled(fleet) {
		alive = c.checkHeartbeats(ctx, gss)
	}

	var errs []error
	now := c.now().Unix()
	var st store.FleetStatus
	// Scale-down / rolling-update bookkeeping, split by spec_hash generation.
	// Reserved servers are never drain candidates.
	var readyCur, readyOld []store.GameServer
	var allocCur, allocOld []store.GameServer
	var activeCur, activeOld int32
	// liveIDs backs the Counter/List fleet aggregation below: every server
	// whose sidecar could plausibly be reporting state.
	var liveIDs []string
	count := func(gs *store.GameServer) {
		st.Total++
		if gs.SpecHash == fleet.SpecHash {
			activeCur++
		} else {
			activeOld++
		}
	}
	for i := range gss {
		gs := &gss[i]
		current := gs.SpecHash == fleet.SpecHash
		switch gs.State {
		case store.StateScheduled, store.StateStarting:
			if now-gs.CreatedAt > int64(c.opts.StartupTimeout/time.Second) {
				// Lost RunTask or a server that never called Ready(); fail
				// it and let the replica shortfall trigger a replacement.
				errs = append(errs, c.failGameServer(ctx, gs, "startup timeout"))
				continue
			}
			count(gs)
			st.Starting++
		case store.StateReady:
			if c.heartbeatExpired(gs, alive, now) {
				c.metrics.HeartbeatTimeout(fleetID)
				errs = append(errs, c.failGameServer(ctx, gs, "heartbeat timeout"))
				continue
			}
			count(gs)
			st.Ready++
			liveIDs = append(liveIDs, gs.ID)
			if current {
				readyCur = append(readyCur, *gs)
			} else {
				readyOld = append(readyOld, *gs)
			}
			// Self-healing: state=Ready but absent from the pool (a lost
			// ZADD on the Ready path) — re-add it.
			errs = append(errs, c.repairPool(ctx, gs, now))
		case store.StateAllocated:
			if c.heartbeatExpired(gs, alive, now) {
				c.metrics.HeartbeatTimeout(fleetID)
				errs = append(errs, c.failGameServer(ctx, gs, "heartbeat timeout"))
				continue
			}
			count(gs)
			st.Allocated++
			liveIDs = append(liveIDs, gs.ID)
			if current {
				allocCur = append(allocCur, *gs)
			} else {
				allocOld = append(allocOld, *gs)
			}
		case store.StateReserved:
			// Safety net for expired reservations: the gateway's timer is
			// primary, this sweep catches gateway failures.
			if gs.ReservedUntil > 0 && now >= gs.ReservedUntil {
				ngs, err := c.store.TransitionState(ctx, gs.ID, store.StateReserved, store.StateReady, nil)
				if err == nil {
					errs = append(errs, c.pool.Add(ctx, fleet.ID, ngs.ID, float64(ngs.ReadyAt), ngs.Labels))
					count(ngs)
					st.Ready++
					liveIDs = append(liveIDs, ngs.ID)
					if current {
						readyCur = append(readyCur, *ngs)
					} else {
						readyOld = append(readyOld, *ngs)
					}
					continue
				}
				if !errors.Is(err, store.ErrConditionFailed) {
					errs = append(errs, err)
				}
			}
			if c.heartbeatExpired(gs, alive, now) {
				c.metrics.HeartbeatTimeout(fleetID)
				errs = append(errs, c.failGameServer(ctx, gs, "heartbeat timeout"))
				continue
			}
			count(gs)
			st.Reserved++
			liveIDs = append(liveIDs, gs.ID)
		case store.StateDraining, store.StateUnhealthy:
			// Keep pushing the task down until the STOPPED event confirms
			// Terminated (one-way flow).
			errs = append(errs, c.stopTask(ctx, gs, "gameserver "+string(gs.State)))
		case store.StateTerminated:
			// DynamoDB TTL garbage-collects these.
		}
	}
	st.Updated = activeCur

	// Counter/List fleet aggregation: sum the Redis-derived
	// per-server snapshots across every live server. Best-effort — a Redis
	// miss just means this reconcile reports stale/empty counters, never a
	// hard failure.
	if snaps, err := c.pool.Counters(ctx, liveIDs); err != nil {
		errs = append(errs, err)
	} else if len(snaps) > 0 {
		st.Counters = aggregateCounters(snaps)
	}

	// Autoscale before comparing: while enabled, the reconciler owns
	// replicas.
	if fleet, err = c.autoscale(ctx, fleet, st); err != nil {
		errs = append(errs, err)
	}

	if activeOld == 0 {
		// Steady state: no old generation in flight.
		switch active := st.Total; {
		case active < fleet.Replicas:
			errs = append(errs, c.scaleUp(ctx, fleet, fleet.Replicas-active))
		case active > fleet.Replicas:
			errs = append(errs, c.scaleDown(ctx, fleet, active-fleet.Replicas, readyCur))
		}
	} else {
		errs = append(errs, c.rollout(ctx, fleet, rolloutView{
			activeCur: activeCur, activeOld: activeOld,
			readyCur: readyCur, readyOld: readyOld,
			allocOld: allocOld,
		}))
	}

	// allocationOverflow: Allocated servers that cannot be
	// drained but exceed the desired world get the configured metadata so
	// the game can exit voluntarily after the session. During an update the
	// desired count for the old generation is zero — all of allocOld
	// qualifies; within the current generation, the excess over replicas.
	if fleet.OverflowJSON != "" {
		targets := allocOld
		if n := int32(len(allocCur)) - fleet.Replicas; n > 0 {
			sort.Slice(allocCur, func(i, j int) bool { return allocCur[i].AllocatedAt < allocCur[j].AllocatedAt })
			targets = append(targets, allocCur[:n]...)
		}
		errs = append(errs, c.applyOverflow(ctx, fleet, targets))
	}

	c.metrics.FleetGameServers(fleetID, st.Total, st.Ready, st.Allocated, st.Starting, st.Reserved, st.Updated)
	if !st.Equal(fleet.Status) {
		err := c.store.UpdateFleetStatus(ctx, fleetID, fleet.Version, st)
		if err != nil && !errors.Is(err, store.ErrVersionConflict) {
			// A conflict means the fleet changed underneath; the next
			// reconcile recomputes from scratch.
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// aggregateCounters sums per-server Counter snapshots into the fleet-wide
// view exposed on FleetStatus.counters. Lists have no
// fleet-level rollup (unbounded value sets) and are server-scoped only.
func aggregateCounters(snaps map[string]pool.Snapshot) map[string]store.CounterAggregate {
	out := map[string]store.CounterAggregate{}
	for _, snap := range snaps {
		for name, c := range snap.Counters {
			agg := out[name]
			agg.Count += c.Count
			agg.Capacity += c.Capacity
			out[name] = agg
		}
	}
	return out
}

// rolloutView is the generation split a reconcile pass observed.
type rolloutView struct {
	activeCur, activeOld int32
	readyCur, readyOld   []store.GameServer
	allocOld             []store.GameServer
}

// rollout advances a rolling update by one idempotent step:
// scale the current generation up within the surge budget, drain old
// Ready servers within the availability budget, and let old
// Allocated/Reserved servers finish their sessions (allocationOverflow
// nudges them separately).
func (c *Controller) rollout(ctx context.Context, fleet *store.Fleet, v rolloutView) error {
	strat := c.fleetStrategy(fleet)
	var errs []error

	if strat.recreate {
		// Recreate: drain the whole old Ready set at once and bring the new
		// generation up to replicas — downtime is the caller's explicit choice.
		errs = append(errs, c.scaleDown(ctx, fleet, int32(len(v.readyOld)), v.readyOld))
		if v.activeCur < fleet.Replicas {
			errs = append(errs, c.scaleUp(ctx, fleet, fleet.Replicas-v.activeCur))
		}
		return errors.Join(errs...)
	}

	// RollingUpdate: launch new-generation servers while total active stays
	// within replicas + surge.
	if v.activeCur < fleet.Replicas {
		room := fleet.Replicas + strat.surge - (v.activeCur + v.activeOld)
		if missing := fleet.Replicas - v.activeCur; missing < room {
			room = missing
		}
		if room > 0 {
			errs = append(errs, c.scaleUp(ctx, fleet, room))
		}
	}

	// Drain old Ready servers (oldest first) as long as total Ready stays at
	// or above replicas - unavailable.
	minReady := fleet.Replicas - strat.unavailable
	budget := int32(len(v.readyCur)+len(v.readyOld)) - minReady
	if budget > int32(len(v.readyOld)) {
		budget = int32(len(v.readyOld))
	}
	if budget > 0 {
		errs = append(errs, c.scaleDown(ctx, fleet, budget, v.readyOld))
	}

	// drainTimeoutSeconds: once the update has been running
	// past the deadline, stop waiting for old-generation sessions and force
	// the remaining old Allocated servers into Draining.
	if strat.drainTimeout > 0 && fleet.GenerationAt > 0 &&
		c.now().Unix() >= fleet.GenerationAt+strat.drainTimeout {
		for i := range v.allocOld {
			gs := &v.allocOld[i]
			if _, err := c.store.TransitionState(ctx, gs.ID, store.StateAllocated, store.StateDraining, nil); err != nil {
				if !errors.Is(err, store.ErrConditionFailed) && !errors.Is(err, store.ErrNotFound) {
					errs = append(errs, err)
				}
				continue
			}
			c.log.Info("rollout drain timeout: force-draining old allocated server",
				"gameserver_id", gs.ID, "fleet_id", fleet.ID)
			errs = append(errs, c.stopTask(ctx, gs, "rollout drain timeout"))
		}
	}
	return errors.Join(errs...)
}

// rolloutStrategy is the parsed fleet update strategy.
type rolloutStrategy struct {
	recreate           bool
	surge, unavailable int32
	// drainTimeout (seconds): force-drain old Allocated servers this long
	// after the generation began; 0 waits indefinitely.
	drainTimeout int64
}

// fleetStrategy parses FleetSpec.strategy, defaulting to RollingUpdate with
// 25% surge / 25% unavailable. Malformed values fall back
// to the defaults with a warning — reconcile must keep making progress.
func (c *Controller) fleetStrategy(fleet *store.Fleet) rolloutStrategy {
	surgeSpec, unavailSpec := "25%", "25%"
	var drainTimeout int64
	if fleet.StrategyJSON != "" {
		s := &arenav1.Strategy{}
		if err := protojson.Unmarshal([]byte(fleet.StrategyJSON), s); err != nil {
			c.log.Warn("bad fleet strategy; using defaults", "fleet_id", fleet.ID, "error", err)
		} else {
			if s.GetType() == arenav1.Strategy_TYPE_RECREATE {
				return rolloutStrategy{recreate: true}
			}
			if v := s.GetRollingUpdate().GetMaxSurge(); v != "" {
				surgeSpec = v
			}
			if v := s.GetRollingUpdate().GetMaxUnavailable(); v != "" {
				unavailSpec = v
			}
			drainTimeout = s.GetRollingUpdate().GetDrainTimeoutSeconds()
		}
	}
	strat := rolloutStrategy{drainTimeout: drainTimeout}
	var ok bool
	if strat.surge, ok = parsePortion(surgeSpec, fleet.Replicas, true); !ok {
		c.log.Warn("bad maxSurge; using 25%", "fleet_id", fleet.ID, "value", surgeSpec)
		strat.surge, _ = parsePortion("25%", fleet.Replicas, true)
	}
	if strat.unavailable, ok = parsePortion(unavailSpec, fleet.Replicas, false); !ok {
		c.log.Warn("bad maxUnavailable; using 25%", "fleet_id", fleet.ID, "value", unavailSpec)
		strat.unavailable, _ = parsePortion("25%", fleet.Replicas, false)
	}
	if strat.surge == 0 && strat.unavailable == 0 {
		// Both zero can never make progress (Kubernetes rejects it at
		// admission; the API layer validates too — this is the backstop).
		strat.unavailable = 1
	}
	return strat
}

// parsePortion resolves a "25%"-style or absolute-count string against a
// total. Percentages round up for surge and down for unavailable, matching
// Kubernetes Deployment semantics.
func parsePortion(s string, total int32, roundUp bool) (int32, bool) {
	if p, isPct := strings.CutSuffix(s, "%"); isPct {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		v := float64(total) * float64(n) / 100
		if roundUp {
			return int32(math.Ceil(v)), true
		}
		return int32(math.Floor(v)), true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// healthDisabled reports whether the fleet template opts out of the
// heartbeat sweep.
func (c *Controller) healthDisabled(fleet *store.Fleet) bool {
	if fleet.TemplateJSON == "" {
		return false
	}
	tmpl := &arenav1.GameServerTemplate{}
	if err := protojson.Unmarshal([]byte(fleet.TemplateJSON), tmpl); err != nil {
		return false
	}
	return tmpl.GetSpec().GetHealth().GetDisabled()
}

// applyOverflow stamps the allocation-overflow labels/annotations on the
// given Allocated servers and pushes the new state to their watch streams.
// Idempotent: servers already carrying every entry are
// skipped; a version conflict is retried by the next pass.
func (c *Controller) applyOverflow(ctx context.Context, fleet *store.Fleet, targets []store.GameServer) error {
	if len(targets) == 0 {
		return nil
	}
	ov := &arenav1.AllocationOverflow{}
	if err := protojson.Unmarshal([]byte(fleet.OverflowJSON), ov); err != nil {
		return fmt.Errorf("fleet %s allocation_overflow: %w", fleet.ID, err)
	}
	var errs []error
	for i := range targets {
		gs := &targets[i]
		if hasAll(gs.Labels, ov.GetLabels()) && hasAll(gs.Annotations, ov.GetAnnotations()) {
			continue
		}
		updated, err := c.store.UpdateGameServerMetadata(ctx, gs.ID, func(g *store.GameServer) {
			if g.Labels == nil && len(ov.GetLabels()) > 0 {
				g.Labels = map[string]string{}
			}
			for k, v := range ov.GetLabels() {
				g.Labels[k] = v
			}
			if g.Annotations == nil && len(ov.GetAnnotations()) > 0 {
				g.Annotations = map[string]string{}
			}
			for k, v := range ov.GetAnnotations() {
				g.Annotations[k] = v
			}
		})
		if err != nil {
			if !errors.Is(err, store.ErrVersionConflict) && !errors.Is(err, store.ErrNotFound) {
				errs = append(errs, err)
			}
			continue
		}
		c.log.Info("allocation overflow metadata applied", "gameserver_id", gs.ID, "fleet_id", fleet.ID)
		// Best-effort watch push; a miss is recovered on sidecar reconnect.
		_ = c.pool.PublishAllocation(ctx, gs.ID, convert.EncodeStatePush(updated))
	}
	return errors.Join(errs...)
}

func hasAll(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// scaleUp creates the missing GameServers, capped per reconcile to smooth
// RunTask bursts; the remainder follows on later passes.
func (c *Controller) scaleUp(ctx context.Context, fleet *store.Fleet, missing int32) error {
	n := int(missing)
	if n > c.opts.MaxLaunchPerReconcile {
		n = c.opts.MaxLaunchPerReconcile
	}
	var errs []error
	for i := 0; i < n; i++ {
		errs = append(errs, c.createGameServer(ctx, fleet))
	}
	c.event(ctx, fleet.ID, store.EventNormal, "ScaleUp", fmt.Sprintf("launching %d gameservers", n))
	return errors.Join(errs...)
}

// event records a fleet-level event; failures only get logged (events are
// observability, not state).
func (c *Controller) event(ctx context.Context, fleetID, eventType, reason, message string) {
	if err := c.store.PutEvent(ctx, store.EventResourceFleet, fleetID, eventType, reason, message); err != nil {
		c.log.Warn("event write failed", "fleet_id", fleetID, "reason", reason, "error", err)
	}
}

// createGameServer persists the Scheduled record first, then launches the
// task idempotently. A RunTask failure leaves the record:
// no RUNNING event follows, and the startup timeout fails it into
// replacement.
func (c *Controller) createGameServer(ctx context.Context, fleet *store.Fleet) error {
	gsID := uuid.NewString()
	gs := store.GameServer{
		ID:        gsID,
		FleetID:   fleet.ID,
		Namespace: fleet.Namespace,
		Name:      fleet.Name + "-" + gsID[:8],
		State:     store.StateScheduled,
		SpecHash:  fleet.SpecHash,
	}
	if fleet.TemplateJSON != "" {
		tmpl := &arenav1.GameServerTemplate{}
		if err := protojson.Unmarshal([]byte(fleet.TemplateJSON), tmpl); err != nil {
			return fmt.Errorf("fleet %s template: %w", fleet.ID, err)
		}
		gs.Labels = tmpl.GetMetadata().GetLabels()
		gs.Annotations = tmpl.GetMetadata().GetAnnotations()
		for _, p := range tmpl.GetSpec().GetPorts() {
			gs.Ports = append(gs.Ports, store.Port{
				Name:     p.GetName(),
				Port:     p.GetContainerPort(), // awsvpc: container port == host port
				Protocol: convert.ProtocolFromProto(p.GetProtocol()),
			})
		}
	}
	if err := c.store.PutGameServer(ctx, gs); err != nil {
		return err
	}
	taskARN, err := c.launcher.Launch(ctx, fleet, gs.ID)
	if err != nil {
		return fmt.Errorf("launch %s: %w", gs.ID, err)
	}
	if taskARN == "" {
		return nil
	}
	// Best-effort write-back; startedBy on the task keeps the linkage even
	// without it.
	_, err = c.store.UpdateGameServerMetadata(ctx, gs.ID, func(g *store.GameServer) {
		if g.TaskARN == "" {
			g.TaskARN = taskARN
		}
	})
	if err != nil && !errors.Is(err, store.ErrVersionConflict) {
		c.log.Warn("task arn write-back failed", "gameserver_id", gs.ID, "error", err)
	}
	return nil
}

// scaleDown drains excess Ready servers, oldest first. Allocated servers are
// never touched; excess Scheduled/Starting servers are
// left to drain on a later pass once they turn Ready.
func (c *Controller) scaleDown(ctx context.Context, fleet *store.Fleet, excess int32, ready []store.GameServer) error {
	sort.Slice(ready, func(i, j int) bool { return ready[i].CreatedAt < ready[j].CreatedAt })
	var errs []error
	drained := 0
	for i := range ready {
		if excess == 0 {
			break
		}
		gs := ready[i]
		if _, err := c.store.TransitionState(ctx, gs.ID, store.StateReady, store.StateDraining, nil); err != nil {
			if errors.Is(err, store.ErrConditionFailed) {
				// Lost the race with an Allocation; that server no longer
				// counts as removable — try the next candidate.
				continue
			}
			errs = append(errs, err)
			continue
		}
		// Transition before pool removal: an allocator popping the stale
		// entry gets a conditional-write rejection and moves on.
		if err := c.pool.Remove(ctx, fleet.ID, gs.ID); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, c.stopTask(ctx, &gs, "scale down"))
		excess--
		drained++
	}
	if drained > 0 {
		c.event(ctx, fleet.ID, store.EventNormal, "ScaleDown", fmt.Sprintf("draining %d gameservers", drained))
	}
	return errors.Join(errs...)
}

// checkHeartbeats bulk-checks liveness for the fleet's Ready/Allocated
// servers. Returns nil when the check could not run (Redis down); callers
// treat nil as "everyone is alive".
func (c *Controller) checkHeartbeats(ctx context.Context, gss []store.GameServer) map[string]bool {
	var ids []string
	for i := range gss {
		if s := gss[i].State; s == store.StateReady || s == store.StateAllocated || s == store.StateReserved {
			ids = append(ids, gss[i].ID)
		}
	}
	if len(ids) == 0 {
		return map[string]bool{}
	}
	alive, err := c.pool.Heartbeats(ctx, ids)
	if err != nil {
		c.log.Warn("heartbeat check failed; skipping health sweep", "error", err)
		return nil
	}
	m := make(map[string]bool, len(ids))
	for i, id := range ids {
		m[id] = alive[i]
	}
	return m
}

// heartbeatExpired reports whether a Ready/Allocated server missed its
// heartbeat, honoring the post-transition grace period.
func (c *Controller) heartbeatExpired(gs *store.GameServer, alive map[string]bool, now int64) bool {
	if alive == nil {
		return false
	}
	anchor := gs.ReadyAt
	if gs.State == store.StateAllocated && gs.AllocatedAt > anchor {
		anchor = gs.AllocatedAt
	}
	if now-anchor < int64(c.opts.HealthGracePeriod/time.Second) {
		return false
	}
	return !alive[gs.ID]
}

// repairPool re-adds a healthy Ready server missing from the pool. Gated on
// the grace period so it does not race the Ready path's own ZADD; racing an
// in-flight allocation is safe — the conditional claim rejects a stale
// entry.
func (c *Controller) repairPool(ctx context.Context, gs *store.GameServer, now int64) error {
	if now-gs.ReadyAt < int64(c.opts.HealthGracePeriod/time.Second) {
		return nil
	}
	pooled, err := c.pool.Contains(ctx, gs.FleetID, gs.ID)
	if err != nil || pooled {
		return err
	}
	c.log.Info("re-adding ready gameserver missing from pool", "gameserver_id", gs.ID, "fleet_id", gs.FleetID)
	return c.pool.Add(ctx, gs.FleetID, gs.ID, float64(gs.ReadyAt), gs.Labels)
}

// failGameServer moves a server to Unhealthy and starts tearing its task
// down. Losing the transition race is fine — whoever won owns the next step.
func (c *Controller) failGameServer(ctx context.Context, gs *store.GameServer, reason string) error {
	if _, err := c.store.TransitionState(ctx, gs.ID, gs.State, store.StateUnhealthy, nil); err != nil {
		if errors.Is(err, store.ErrConditionFailed) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if gs.State == store.StateReady {
		// It may still sit in the pool; remove before an allocator pops it
		// (the conditional claim would reject it anyway).
		if err := c.pool.Remove(ctx, gs.FleetID, gs.ID); err != nil {
			c.log.Warn("pool remove failed", "gameserver_id", gs.ID, "error", err)
		}
	}
	c.log.Info("gameserver unhealthy", "gameserver_id", gs.ID, "fleet_id", gs.FleetID, "reason", reason)
	c.metrics.UnhealthyGameServer(gs.FleetID)
	return c.stopTask(ctx, gs, reason)
}

// stopTask asks ECS to stop the server's task; the STOPPED event confirms
// Terminated. A server that never got a task (RunTask lost) has no event to
// wait for, so it is confirmed Terminated directly.
func (c *Controller) stopTask(ctx context.Context, gs *store.GameServer, reason string) error {
	if gs.TaskARN == "" {
		_, err := c.store.MarkTerminated(ctx, gs.ID)
		if errors.Is(err, store.ErrConditionFailed) || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	return c.launcher.Stop(ctx, gs.TaskARN, reason)
}
