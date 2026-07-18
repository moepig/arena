// Package allocation implements the Allocator: the
// lock-free fast path (Redis ZPOPMIN → DynamoDB TransactWriteItems with a
// conditional Ready → Allocated transition, bounded claim retries) and the
// selector slow path (fleet-index GSI query + conditional claim + ZREM).
// Allocation IDs are derived from the client idempotency key (UUIDv5).
package allocation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
	"github.com/moepig/arena/internal/telemetry"
)

// maxClaimAttempts bounds the ZPOPMIN → conditional-claim loop. Condition
// failures are rare (candidate went Unhealthy between pop and claim), so a
// handful of retries is plenty; beyond that we tell the caller to retry.
const maxClaimAttempts = 4

// allocationNamespace is the UUIDv5 namespace for idempotency keys.
var allocationNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // uuid.NameSpaceDNS-compatible fixed value

// AllocationID derives the allocation ID from a client idempotency key.
// Same key → same ID, so resends converge on one record.
func AllocationID(idempotencyKey string) string {
	return uuid.NewSHA1(allocationNamespace, []byte("arena-alloc:"+idempotencyKey)).String()
}

// Store is the DynamoDB surface the allocator needs.
type Store interface {
	GetAllocation(ctx context.Context, allocID string) (*store.Allocation, error)
	GetGameServer(ctx context.Context, gsID string) (*store.GameServer, error)
	ClaimGameServer(ctx context.Context, gsID string, alloc store.Allocation, mutate func(*store.GameServer)) (*store.GameServer, error)
	ListAllGameServersByFleet(ctx context.Context, fleetID string, state store.State) ([]store.GameServer, error)
	TransitionState(ctx context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error)
	ReleaseAllocation(ctx context.Context, allocID string) error
	// AddAllocation commits an additional Allocation record for a GameServer
	// that stays Allocated (high-density reallocation).
	AddAllocation(ctx context.Context, gsID string, alloc store.Allocation) (*store.Allocation, error)
}

// Pool is the Redis surface the allocator needs.
type Pool interface {
	PopMin(ctx context.Context, fleetID string) (string, error)
	// PopMinSelector pops from the sub-pool of servers pooled with exactly
	// this label set; pool.ErrEmpty on a miss.
	PopMinSelector(ctx context.Context, fleetID string, labels map[string]string) (string, error)
	Add(ctx context.Context, fleetID, gsID string, score float64, labels map[string]string) error
	Remove(ctx context.Context, fleetID, gsID string) error
	PublishAllocation(ctx context.Context, gsID string, payload []byte) error
	// Counters bulk-fetches Counter/List snapshots for counter filters.
	Counters(ctx context.Context, ids []string) (map[string]pool.Snapshot, error)
	// ReserveCounter / ReleaseCounter guard the high-density realloc race.
	ReserveCounter(ctx context.Context, fleetID, name, gsID string, amount int64) (bool, error)
	ReleaseCounter(ctx context.Context, fleetID, name, gsID string, amount int64) error
}

// Operator is a selector requirement operator.
type Operator string

// Requirement operators.
const (
	OpEquals    Operator = "Equals"
	OpNotEquals Operator = "NotEquals"
	OpIn        Operator = "In"
	OpNotIn     Operator = "NotIn"
	OpExists    Operator = "Exists"
	OpNotExists Operator = "NotExists"
)

// Requirement is one label requirement of a selector.
type Requirement struct {
	Key      string
	Operator Operator
	Values   []string
}

// Selector narrows candidate GameServers. All parts AND together;
// Preferred never disqualifies, it only orders candidates.
type Selector struct {
	// MatchLabels: exact match on every entry.
	MatchLabels map[string]string
	// MatchFields: supported keys "id", "spec_hash".
	MatchFields map[string]string
	// Required requirements (all must match).
	Required []Requirement
	// Preferred requirements: candidates matching more of these are tried
	// first (ties broken oldest-Ready-first).
	Preferred []Requirement
}

// empty reports whether the selector matches everything (no constraints and
// no preference ordering).
func (s Selector) empty() bool {
	return len(s.MatchLabels) == 0 && len(s.MatchFields) == 0 && len(s.Required) == 0 && len(s.Preferred) == 0
}

// Request is a resolved allocation request (fleet already looked up).
type Request struct {
	AllocationID string
	FleetID      string
	SessionID    string
	Metadata     map[string]string
	// Selectors is the ordered fallback chain: tried first
	// to last, first selector with a claimable match wins. Non-empty routes
	// to the slow path.
	Selectors []Selector
	// PatchLabels/PatchAnnotations are applied to the GameServer in the same
	// transaction that commits the allocation.
	PatchLabels      map[string]string
	PatchAnnotations map[string]string
	// CounterFilters / Priorities / AllowAllocated route to the Counter-aware
	// path. Non-empty CounterFilters or Priorities
	// takes precedence over Selectors-only routing; Selectors, if also set,
	// narrow candidates further (a candidate must match at least one).
	CounterFilters []CounterFilter
	Priorities     []Priority
	AllowAllocated bool
}

// CounterFilter narrows candidates by a named Counter's available capacity
// (= capacity - count). MinAvailable defaults to 1 when zero (a filter
// admitting full servers is useless); MaxAvailable 0 means unbounded.
type CounterFilter struct {
	Name                       string
	MinAvailable, MaxAvailable int64
}

// PriorityOrder ranks candidates by Counter available capacity.
type PriorityOrder int

// Priority orders.
const (
	PriorityUnspecified PriorityOrder = iota
	// PriorityAscending packs sessions onto the fewest servers (least
	// available capacity first).
	PriorityAscending
	// PriorityDescending spreads sessions across servers (most available
	// capacity first).
	PriorityDescending
)

// Priority orders eligible candidates by one Counter's available capacity.
// Earlier entries break ties from later ones.
type Priority struct {
	Counter string
	Order   PriorityOrder
}

// Result is a committed allocation.
type Result struct {
	Allocation store.Allocation
	GameServer store.GameServer
	// Reused is true when the idempotency key matched an existing record.
	Reused bool
}

// Allocator runs the allocation paths against the store and pool.
type Allocator struct {
	store Store
	pool  Pool
	// publish encodes the payload pushed to the sidecar via pub/sub.
	// Injected so the wire format lives with the gateway, not here.
	encodePush func(gs *store.GameServer, alloc *store.Allocation) []byte
	metrics    *telemetry.Metrics
}

// New returns an Allocator. encodePush may be nil (no sidecar push).
func New(s Store, p Pool, encodePush func(*store.GameServer, *store.Allocation) []byte) *Allocator {
	return &Allocator{store: s, pool: p, encodePush: encodePush}
}

// SetMetrics attaches the allocation metric set (nil = no-op).
func (a *Allocator) SetMetrics(m *telemetry.Metrics) { a.metrics = m }

// Allocate claims a Ready GameServer.
// Error codes: RESOURCE_EXHAUSTED when the fleet has no Ready inventory,
// ABORTED when claim contention exhausted the retry budget.
func (a *Allocator) Allocate(ctx context.Context, req Request) (res *Result, err error) {
	defer func(start time.Time) {
		a.metrics.Allocation(req.FleetID, time.Since(start),
			connect.CodeOf(err) == connect.CodeResourceExhausted, err != nil)
	}(time.Now())
	return a.allocate(ctx, req)
}

func (a *Allocator) allocate(ctx context.Context, req Request) (*Result, error) {
	// 0. Idempotency: an existing record wins (safe client resend).
	if existing, err := a.store.GetAllocation(ctx, req.AllocationID); err == nil {
		gs, err := a.store.GetGameServer(ctx, existing.GameServerID)
		if err != nil {
			return nil, err
		}
		return &Result{Allocation: *existing, GameServer: *gs, Reused: true}, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	if len(req.CounterFilters) > 0 || len(req.Priorities) > 0 {
		return a.allocateCounter(ctx, req)
	}
	for _, sel := range req.Selectors {
		if !sel.empty() {
			return a.allocateSelector(ctx, req)
		}
	}
	// No selectors (or only match-everything ones): lock-free fast path.
	return a.allocateFast(ctx, req)
}

// allocateFast is the lock-free hot path: ZPOPMIN pops a candidate no other
// request can also pop; the conditional transaction is the last line of
// defense against races with Unhealthy transitions.
func (a *Allocator) allocateFast(ctx context.Context, req Request) (*Result, error) {
	for attempt := 0; attempt < maxClaimAttempts; attempt++ {
		gsID, err := a.pool.PopMin(ctx, req.FleetID)
		if errors.Is(err, pool.ErrEmpty) {
			// A racing resend of this idempotency key may have committed
			// (and drained the pool) after our step-0 check — resends must
			// converge on that allocation, not see RESOURCE_EXHAUSTED.
			if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
				return res, nil
			}
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("no ready gameserver"))
		}
		if err != nil {
			return nil, err
		}

		res, err := a.claim(ctx, gsID, req)
		if errors.Is(err, store.ErrConditionFailed) {
			// Two distinct races collapse into this error: (a) a racing
			// resend of the same key already committed — return its result
			// and put the still-Ready candidate back in the pool, or (b)
			// the candidate left Ready (Unhealthy etc.) — drop it and try
			// the next one.
			if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
				a.requeue(ctx, req.FleetID, gsID)
				return res, nil
			}
			continue
		}
		return res, err
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("claim contention, retry"))
}

// existingAllocation resolves an idempotency key that already committed.
func (a *Allocator) existingAllocation(ctx context.Context, allocID string) (*Result, bool) {
	existing, err := a.store.GetAllocation(ctx, allocID)
	if err != nil {
		return nil, false
	}
	gs, err := a.store.GetGameServer(ctx, existing.GameServerID)
	if err != nil {
		return nil, false
	}
	return &Result{Allocation: *existing, GameServer: *gs, Reused: true}, true
}

// requeue returns a popped-but-unclaimed candidate to the pool, provided it
// is still Ready. Best-effort: a lost requeue is repaired by the health
// reconciler's "Ready but absent from pool" sweep.
func (a *Allocator) requeue(ctx context.Context, fleetID, gsID string) {
	gs, err := a.store.GetGameServer(ctx, gsID)
	if err != nil || gs.State != store.StateReady {
		return
	}
	_ = a.pool.Add(ctx, fleetID, gsID, float64(gs.ReadyAt), gs.Labels)
}

// allocateSelector is the slow path:
// query Ready servers from the fleet-index GSI, walk the selector fallback
// chain in order, claim conditionally, and only then remove the winner from
// the pool. Within one selector, candidates matching more `preferred`
// requirements go first, ties broken oldest-Ready-first.
func (a *Allocator) allocateSelector(ctx context.Context, req Request) (*Result, error) {
	var ready []store.GameServer
	loaded := false
	attempts := 0
	for _, sel := range req.Selectors {
		// Label-only selectors first try the exact-label-set sub-pool —
		// a Redis pop instead of a GSI query. A miss (cold
		// sub-pool, extra constraints, or label superset matches) falls
		// through to the GSI scan, so the sub-pool is purely an accelerator.
		if sel.labelOnly() {
			res, done, err := a.allocateSubpool(ctx, req, sel, &attempts)
			if done {
				return res, err
			}
		}
		if !loaded {
			var err error
			if ready, err = a.store.ListAllGameServersByFleet(ctx, req.FleetID, store.StateReady); err != nil {
				return nil, err
			}
			loaded = true
		}
		for _, gs := range sel.order(sel.filter(ready)) {
			if attempts++; attempts > maxClaimAttempts {
				return nil, connect.NewError(connect.CodeAborted, errors.New("claim contention, retry"))
			}
			res, err := a.claim(ctx, gs.ID, req)
			if errors.Is(err, store.ErrConditionFailed) {
				// Same disambiguation as the fast path; the candidate was never
				// popped here, so there is nothing to requeue.
				if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
					return res, nil
				}
				continue // claimed or moved concurrently; next match
			}
			if err != nil {
				return nil, err
			}
			// Claimed via GSI, so the entry is still pooled — remove it.
			if err := a.pool.Remove(ctx, req.FleetID, gs.ID); err != nil {
				// Best-effort: a stale pool entry is rejected by the fast
				// path's conditional claim and dropped there.
				_ = err
			}
			return res, nil
		}
	}
	return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("no ready gameserver matching selectors"))
}

// labelOnly reports whether the selector can be served by the exact-label
// sub-pool: match_labels present, nothing else.
func (s Selector) labelOnly() bool {
	return len(s.MatchLabels) > 0 && len(s.MatchFields) == 0 && len(s.Required) == 0 && len(s.Preferred) == 0
}

// allocateSubpool drains candidates from the selector sub-pool until a
// claim succeeds, the attempt budget runs out, or the sub-pool misses.
// done=false means "fall back to the GSI scan for this selector".
// Popped entries may be stale (label change, state change since pooling);
// they are verified against DynamoDB before claiming and dropped otherwise
// — the pop itself already removed them from the sub-pool.
func (a *Allocator) allocateSubpool(ctx context.Context, req Request, sel Selector, attempts *int) (*Result, bool, error) {
	for {
		if *attempts >= maxClaimAttempts {
			return nil, true, connect.NewError(connect.CodeAborted, errors.New("claim contention, retry"))
		}
		gsID, err := a.pool.PopMinSelector(ctx, req.FleetID, sel.MatchLabels)
		if err != nil {
			return nil, false, nil // empty or Redis trouble: GSI path decides
		}
		gs, err := a.store.GetGameServer(ctx, gsID)
		if err != nil || gs.State != store.StateReady || !labelsMatch(gs.Labels, sel.MatchLabels) {
			continue // stale sub-pool entry; already popped, just drop it
		}
		*attempts++
		res, err := a.claim(ctx, gsID, req)
		if errors.Is(err, store.ErrConditionFailed) {
			// Same disambiguation as the fast path (racing resend vs. lost
			// candidate).
			if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
				a.requeue(ctx, req.FleetID, gsID)
				return res, true, nil
			}
			continue
		}
		if err != nil {
			return nil, true, err
		}
		// The main-pool entry is still present — remove it.
		_ = a.pool.Remove(ctx, req.FleetID, gsID)
		return res, true, nil
	}
}

// allocateCounter serves counter_filters / priorities and,
// with AllowAllocated, the high-density reallocation path. Both are
// slow-path: ranking the whole candidate set by available Counter capacity
// can't be expressed as a single ZPOPMIN, so this loads candidates from the
// fleet-index GSI (Ready, plus Allocated when AllowAllocated) and their
// Counter snapshots from Redis, then walks the ranked list claiming the
// first that still succeeds.
func (a *Allocator) allocateCounter(ctx context.Context, req Request) (*Result, error) {
	states := []store.State{store.StateReady}
	if req.AllowAllocated {
		states = append(states, store.StateAllocated)
	}
	var candidates []store.GameServer
	for _, st := range states {
		gss, err := a.store.ListAllGameServersByFleet(ctx, req.FleetID, st)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, gss...)
	}
	if len(req.Selectors) > 0 {
		candidates = slices.DeleteFunc(candidates, func(gs store.GameServer) bool {
			return !selectorsMatchAny(req.Selectors, gs)
		})
	}
	if len(candidates) == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("no gameserver matching selectors"))
	}

	ids := make([]string, len(candidates))
	for i, gs := range candidates {
		ids[i] = gs.ID
	}
	snaps, err := a.pool.Counters(ctx, ids)
	if err != nil {
		return nil, err
	}

	elig := eligibleCounterCandidates(candidates, snaps, req.CounterFilters, req.Priorities)
	if len(elig) == 0 {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("no gameserver satisfies counter_filters"))
	}

	attempts := 0
	for _, c := range elig {
		if attempts++; attempts > maxClaimAttempts {
			return nil, connect.NewError(connect.CodeAborted, errors.New("claim contention, retry"))
		}
		if c.gs.State == store.StateAllocated {
			res, ok, err := a.claimAdditional(ctx, req, c.gs)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // lost the reservation race; try the next candidate
			}
			return res, nil
		}
		res, err := a.claim(ctx, c.gs.ID, req)
		if errors.Is(err, store.ErrConditionFailed) {
			if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
				return res, nil
			}
			continue // candidate moved concurrently; try the next one
		}
		if err != nil {
			return nil, err
		}
		if err := a.pool.Remove(ctx, req.FleetID, c.gs.ID); err != nil {
			_ = err // best-effort, same as the selector path
		}
		return res, nil
	}
	return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("no gameserver satisfies counter_filters"))
}

// counterCandidate pairs a GameServer with the available capacity of every
// counter its filters/priorities reference (avoids re-deriving it per sort
// comparison).
type counterCandidate struct {
	gs        store.GameServer
	available map[string]int64
}

// eligibleCounterCandidates filters candidates to those with a Counter
// snapshot satisfying every filter (no snapshot = excluded, the safe-side
// default for recovering Counter data), then ranks them by
// Priorities (earlier entries break ties from later ones), falling back to
// oldest-Ready-first.
func eligibleCounterCandidates(candidates []store.GameServer, snaps map[string]pool.Snapshot, filters []CounterFilter, priorities []Priority) []counterCandidate {
	var elig []counterCandidate
	for _, gs := range candidates {
		snap, ok := snaps[gs.ID]
		if !ok {
			continue
		}
		available := map[string]int64{}
		ok = true
		for _, f := range filters {
			c, present := snap.Counters[f.Name]
			if !present {
				ok = false
				break
			}
			avail := c.Capacity - c.Count
			min := f.MinAvailable
			if min == 0 {
				min = 1
			}
			if avail < min || (f.MaxAvailable > 0 && avail > f.MaxAvailable) {
				ok = false
				break
			}
			available[f.Name] = avail
		}
		if !ok {
			continue
		}
		for _, p := range priorities {
			if _, have := available[p.Counter]; have {
				continue
			}
			if c, present := snap.Counters[p.Counter]; present {
				available[p.Counter] = c.Capacity - c.Count
			}
		}
		elig = append(elig, counterCandidate{gs: gs, available: available})
	}
	sort.SliceStable(elig, func(i, j int) bool {
		for _, p := range priorities {
			ai, aj := elig[i].available[p.Counter], elig[j].available[p.Counter]
			if ai == aj {
				continue
			}
			if p.Order == PriorityDescending {
				return ai > aj
			}
			return ai < aj
		}
		return elig[i].gs.ReadyAt < elig[j].gs.ReadyAt
	})
	return elig
}

// selectorsMatchAny reports whether gs satisfies at least one selector in
// the chain (an empty chain always matches). Filtering on "any" rather than
// walking the fallback-chain cascade collapses naturally into the single
// ranked pass allocateCounter needs.
func selectorsMatchAny(sels []Selector, gs store.GameServer) bool {
	if len(sels) == 0 {
		return true
	}
	for _, sel := range sels {
		if len(sel.filter([]store.GameServer{gs})) == 1 {
			return true
		}
	}
	return false
}

// claimAdditional commits a high-density reallocation: an
// Allocation record for a GameServer that stays Allocated. ok=false means
// the Counter reservation or the claim lost a race; the caller moves to the
// next candidate rather than treating it as a hard error.
func (a *Allocator) claimAdditional(ctx context.Context, req Request, gs store.GameServer) (*Result, bool, error) {
	reserved := make([]string, 0, len(req.CounterFilters))
	rollback := func() {
		for _, name := range reserved {
			_ = a.pool.ReleaseCounter(ctx, req.FleetID, name, gs.ID, 1)
		}
	}
	for _, f := range req.CounterFilters {
		ok, err := a.pool.ReserveCounter(ctx, req.FleetID, f.Name, gs.ID, 1)
		if err != nil {
			rollback()
			return nil, false, err
		}
		if !ok {
			rollback()
			return nil, false, nil
		}
		reserved = append(reserved, f.Name)
	}

	alloc := store.Allocation{
		ID: req.AllocationID, SessionID: req.SessionID, Metadata: req.Metadata,
		Additional: true, ReservedCounters: slices.Clone(reserved),
	}
	committed, err := a.store.AddAllocation(ctx, gs.ID, alloc)
	if errors.Is(err, store.ErrConditionFailed) {
		rollback()
		if res, ok := a.existingAllocation(ctx, req.AllocationID); ok {
			return res, true, nil
		}
		return nil, false, nil
	}
	if err != nil {
		rollback()
		return nil, false, err
	}
	if a.encodePush != nil {
		_ = a.pool.PublishAllocation(ctx, gs.ID, a.encodePush(&gs, committed))
	}
	return &Result{Allocation: *committed, GameServer: gs}, true, nil
}

// filter returns the candidates satisfying MatchLabels, MatchFields and
// Required.
func (s Selector) filter(gss []store.GameServer) []store.GameServer {
	var out []store.GameServer
	for _, gs := range gss {
		if !labelsMatch(gs.Labels, s.MatchLabels) {
			continue
		}
		if !fieldsMatch(&gs, s.MatchFields) {
			continue
		}
		ok := true
		for _, r := range s.Required {
			ok = ok && r.matches(gs.Labels)
		}
		if ok {
			out = append(out, gs)
		}
	}
	return out
}

// order sorts candidates by preferred-match count (desc), then oldest Ready
// first — the same age order the fast path's ZPOPMIN provides.
func (s Selector) order(gss []store.GameServer) []store.GameServer {
	score := func(gs *store.GameServer) int {
		n := 0
		for _, r := range s.Preferred {
			if r.matches(gs.Labels) {
				n++
			}
		}
		return n
	}
	sort.SliceStable(gss, func(i, j int) bool {
		si, sj := score(&gss[i]), score(&gss[j])
		if si != sj {
			return si > sj
		}
		return gss[i].ReadyAt < gss[j].ReadyAt
	})
	return gss
}

func (r Requirement) matches(labels map[string]string) bool {
	v, exists := labels[r.Key]
	switch r.Operator {
	case OpEquals:
		return exists && len(r.Values) > 0 && v == r.Values[0]
	case OpNotEquals:
		return !exists || len(r.Values) == 0 || v != r.Values[0]
	case OpIn:
		return exists && slices.Contains(r.Values, v)
	case OpNotIn:
		return !exists || !slices.Contains(r.Values, v)
	case OpExists:
		return exists
	case OpNotExists:
		return !exists
	default:
		return false
	}
}

// fieldsMatch evaluates MatchFields ("id", "spec_hash").
// Unknown keys never match; the API layer validates them up front.
func fieldsMatch(gs *store.GameServer, fields map[string]string) bool {
	for k, v := range fields {
		switch k {
		case "id":
			if gs.ID != v {
				return false
			}
		case "spec_hash":
			if gs.SpecHash != v {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// claim commits Ready → Allocated + the Allocation record atomically —
// including the game_server_metadata patch — and pushes
// the allocation to the sidecar (best-effort).
func (a *Allocator) claim(ctx context.Context, gsID string, req Request) (*Result, error) {
	alloc := store.Allocation{
		ID:        req.AllocationID,
		SessionID: req.SessionID,
		Metadata:  req.Metadata,
	}
	var mutate func(*store.GameServer)
	if len(req.PatchLabels) > 0 || len(req.PatchAnnotations) > 0 {
		mutate = func(gs *store.GameServer) {
			if gs.Labels == nil && len(req.PatchLabels) > 0 {
				gs.Labels = map[string]string{}
			}
			for k, v := range req.PatchLabels {
				gs.Labels[k] = v
			}
			if gs.Annotations == nil && len(req.PatchAnnotations) > 0 {
				gs.Annotations = map[string]string{}
			}
			for k, v := range req.PatchAnnotations {
				gs.Annotations[k] = v
			}
		}
	}
	gs, err := a.store.ClaimGameServer(ctx, gsID, alloc, mutate)
	if err != nil {
		return nil, err
	}
	alloc.GameServerID = gs.ID
	alloc.FleetID = gs.FleetID
	alloc.AllocatedAt = gs.AllocatedAt

	if a.encodePush != nil {
		// Best-effort push; a miss is recovered on sidecar reconnect.
		_ = a.pool.PublishAllocation(ctx, gs.ID, a.encodePush(gs, &alloc))
	}
	return &Result{Allocation: alloc, GameServer: *gs}, nil
}

// Release ends an allocation: Allocated → Ready (reuse policy) and the
// server re-enters the pool. Order matters — commit the transition before
// pooling, exactly like the Ready path.
func (a *Allocator) Release(ctx context.Context, allocID string) error {
	alloc, err := a.store.GetAllocation(ctx, allocID)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("allocation %s not found", allocID))
	}
	if err != nil {
		return err
	}

	if err := a.store.ReleaseAllocation(ctx, allocID); err != nil {
		return err
	}
	for _, name := range alloc.ReservedCounters {
		_ = a.pool.ReleaseCounter(ctx, alloc.FleetID, name, alloc.GameServerID, 1)
	}
	if alloc.Additional {
		// High-density session end: the GameServer's
		// Allocated/Ready lifecycle is governed by the primary allocation
		// only, never by an add-on session ending.
		return nil
	}

	gs, err := a.store.TransitionState(ctx, alloc.GameServerID, store.StateAllocated, store.StateReady, nil)
	if errors.Is(err, store.ErrConditionFailed) {
		// Already moved (Unhealthy / Draining / re-released): nothing to pool.
		return nil
	}
	if err != nil {
		return err
	}
	return a.pool.Add(ctx, gs.FleetID, gs.ID, float64(gs.ReadyAt), gs.Labels)
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
