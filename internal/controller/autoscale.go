package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

// scheduleLookback bounds the backward scan for the Schedule policy: the
// entry that fired most recently within this window owns the replica count.
const scheduleLookback = 24*time.Hour + time.Minute

// autoscale recomputes replicas for a fleet that owns autoscaling.
// It runs inside reconcileFleet, so the scale decision
// and the reconcile acting on it are serialized per fleet by the work
// queue — the "autoscaler races reconcile" conflict cannot occur.
// Returns the (possibly updated) fleet the rest of the pass should use.
func (c *Controller) autoscale(ctx context.Context, fleet *store.Fleet, st store.FleetStatus) (*store.Fleet, error) {
	if !fleet.AutoscalingEnabled || fleet.AutoscalingJSON == "" {
		return fleet, nil
	}
	as := &arenav1.Autoscaling{}
	if err := protojson.Unmarshal([]byte(fleet.AutoscalingJSON), as); err != nil {
		return fleet, fmt.Errorf("fleet %s autoscaling: %w", fleet.ID, err)
	}

	desired, ok, err := c.desiredReplicas(ctx, as.GetPolicy(), fleet, st, false)
	if err != nil || !ok {
		return fleet, err
	}
	desired = clamp(desired, as.GetMinReplicas(), as.GetMaxReplicas())
	c.metrics.AutoscaleDesired(fleet.ID, desired, fleet.Replicas)
	if desired == fleet.Replicas {
		return fleet, nil
	}

	updated := *fleet
	updated.Replicas = desired
	// Version-conditioned write; a conflict (user changed spec mid-pass)
	// just defers to the next cycle's recomputation.
	after, err := c.store.UpdateFleet(ctx, updated)
	if errors.Is(err, store.ErrVersionConflict) {
		return fleet, nil
	}
	if err != nil {
		return fleet, err
	}
	c.log.Info("autoscaled fleet", "fleet_id", fleet.ID, "replicas", desired, "was", fleet.Replicas)
	return after, nil
}

// desiredReplicas evaluates one policy. ok=false means "no opinion, keep
// the current count" (unset policy, failed webhook, a schedule that never
// fired in the lookback window, or a counter with no data yet). nested
// guards against Chain-in-Chain, which validation rejects anyway.
func (c *Controller) desiredReplicas(ctx context.Context, p *arenav1.AutoscalingPolicy, fleet *store.Fleet, st store.FleetStatus, nested bool) (int32, bool, error) {
	switch p.GetType() {
	case arenav1.AutoscalingPolicy_TYPE_BUFFER:
		return bufferDesired(p.GetBuffer(), st.Allocated), true, nil

	case arenav1.AutoscalingPolicy_TYPE_SCHEDULE:
		return c.scheduleDesired(p.GetSchedule(), fleet.Replicas)

	case arenav1.AutoscalingPolicy_TYPE_WEBHOOK:
		desired, ok, err := c.webhooks.desired(ctx, p.GetWebhook(), fleet, st)
		if err != nil {
			// Failure keeps the current count.
			c.log.Warn("webhook autoscaler failed; keeping replicas", "fleet_id", fleet.ID, "error", err)
			return 0, false, nil
		}
		return desired, ok, nil

	case arenav1.AutoscalingPolicy_TYPE_COUNTER:
		d, ok := counterDesired(p.GetCounter(), st, fleet.Replicas)
		return d, ok, nil

	case arenav1.AutoscalingPolicy_TYPE_CHAIN:
		if nested {
			return 0, false, fmt.Errorf("fleet %s: chain inside chain", fleet.ID)
		}
		return c.chainDesired(ctx, p.GetChain(), fleet, st)

	default:
		return 0, false, nil
	}
}

// chainDesired applies the first entry whose schedule window covers "now";
// an entry without a schedule always matches.
func (c *Controller) chainDesired(ctx context.Context, entries []*arenav1.ChainEntry, fleet *store.Fleet, st store.FleetStatus) (int32, bool, error) {
	now := c.now()
	for _, e := range entries {
		if sched := e.GetSchedule(); sched != nil {
			expr, err := parseCron(sched.GetCron())
			if err != nil {
				return 0, false, err
			}
			window := time.Duration(sched.GetDurationSeconds()) * time.Second
			if window <= 0 {
				window = time.Hour
			}
			if _, ok := expr.lastMatch(now, window); !ok {
				continue // window not active
			}
		}
		return c.desiredReplicas(ctx, e.GetPolicy(), fleet, st, true)
	}
	return 0, false, nil
}

// counterDesired scales on aggregate Counter capacity:
// keep `buffer` free capacity available, assuming the current per-server
// capacity as the marginal contribution of a replica. No data (fleet not
// reporting counters yet) = no opinion, which is the safe direction.
func counterDesired(p *arenav1.CounterPolicy, st store.FleetStatus, current int32) (int32, bool) {
	agg, ok := st.Counters[p.GetKey()]
	if !ok || current <= 0 || st.Total <= 0 || agg.Capacity <= 0 {
		return 0, false
	}
	perGS := agg.Capacity / int64(st.Total)
	if perGS <= 0 {
		return 0, false
	}
	buffer := p.GetBufferSize()
	if buffer == 0 && p.GetBufferPercent() > 0 {
		buffer = (agg.Capacity*int64(p.GetBufferPercent()) + 99) / 100 // ceil
	}
	if buffer <= 0 {
		return 0, false
	}
	available := agg.Capacity - agg.Count
	delta := buffer - available
	switch {
	case delta > 0: // short on free capacity: scale up, rounding up
		return current + int32((delta+perGS-1)/perGS), true
	case delta < 0: // surplus: scale down, rounding down (conservative)
		return current - int32((-delta)/perGS), true
	default:
		return current, true
	}
}

// bufferDesired: desired = allocated + buffer, where buffer is the absolute
// buffer_size or buffer_percent of the allocated count (at least 1 so an
// idle fleet still holds inventory).
func bufferDesired(b *arenav1.BufferPolicy, allocated int32) int32 {
	buffer := b.GetBufferSize()
	if buffer == 0 && b.GetBufferPercent() > 0 {
		buffer = (allocated*b.GetBufferPercent() + 99) / 100 // ceil
		if buffer < 1 {
			buffer = 1
		}
	}
	return allocated + buffer
}

// scheduleDesired: the schedule entry that fired most recently (within the
// lookback window) owns the replica count.
func (c *Controller) scheduleDesired(entries []*arenav1.SchedulePolicy, current int32) (int32, bool, error) {
	now := c.now()
	var best time.Time
	desired := current
	found := false
	for _, e := range entries {
		expr, err := parseCron(e.GetCron())
		if err != nil {
			return 0, false, err
		}
		if t, ok := expr.lastMatch(now, scheduleLookback); ok && t.After(best) {
			best, desired, found = t, e.GetReplicas(), true
		}
	}
	return desired, found, nil
}

func clamp(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if hi > 0 && v > hi {
		return hi
	}
	return v
}
