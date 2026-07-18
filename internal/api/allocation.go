package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/store"
)

// fleetSelectorCacheTTL and maxFleetSelectorFleets bound cross-fleet
// allocation: a full namespace drain per Allocate call
// would blow the hot-path latency budget, and "hundreds of fleets behind
// one selector" gives up the fast path's latency guarantees entirely.
const (
	fleetSelectorCacheTTL  = 60 * time.Second
	maxFleetSelectorFleets = 8
)

// AllocationStore is the DynamoDB surface the allocation API layer needs.
type AllocationStore interface {
	GetFleetByName(ctx context.Context, namespace, name string) (*store.Fleet, error)
	GetAllocation(ctx context.Context, allocID string) (*store.Allocation, error)
	// ListAllFleetsByNamespace resolves fleet_selector candidates; cached,
	// see fleetSelectorCacheTTL.
	ListAllFleetsByNamespace(ctx context.Context, namespace string) ([]store.Fleet, error)
}

// AllocationServer implements arena.v1.AllocationService. The hot path
// lives in internal/allocation; this layer resolves the fleet(s) and maps
// the wire types.
type AllocationServer struct {
	arenav1connect.UnimplementedAllocationServiceHandler
	store     AllocationStore
	allocator *allocation.Allocator

	fleetSelectorMu    sync.Mutex
	fleetSelectorCache map[string]fleetSelectorCacheEntry
	// now is overridden in tests; nil means time.Now.
	now func() time.Time
}

type fleetSelectorCacheEntry struct {
	fleets    []store.Fleet
	expiresAt time.Time
}

func (s *AllocationServer) timeNow() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *AllocationServer) Allocate(ctx context.Context, req *connect.Request[arenav1.AllocateRequest]) (*connect.Response[arenav1.AllocateResponse], error) {
	m := req.Msg
	if m.GetIdempotencyKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key is required"))
	}
	hasName, hasSelector := m.GetFleetName() != "", len(m.GetFleetSelector()) > 0
	if hasName == hasSelector {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("exactly one of fleet_name or fleet_selector is required"))
	}
	selectors, err := selectorsFromProto(m.GetSelectors())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	counterFilters, err := counterFiltersFromProto(m.GetCounterFilters())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	priorities, err := prioritiesFromProto(m.GetPriorities())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if m.GetAllowAllocated() && len(counterFilters) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("allow_allocated requires at least one counter_filter"))
	}

	namespace := namespaceOrDefault(m.GetNamespace())
	var fleets []store.Fleet
	if hasName {
		fleet, err := s.store.GetFleetByName(ctx, namespace, m.GetFleetName())
		if err != nil {
			return nil, asConnectError(err)
		}
		fleets = []store.Fleet{*fleet}
	} else {
		fleets, err = s.resolveFleetSelector(ctx, namespace, m.GetFleetSelector())
		if err != nil {
			return nil, asConnectError(err)
		}
		if len(fleets) > maxFleetSelectorFleets {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("fleet_selector matches %d fleets, want at most %d", len(fleets), maxFleetSelectorFleets))
		}
		if len(fleets) == 0 {
			return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("fleet_selector matches no fleet"))
		}
	}

	// Sequential fast-path fallback across fleets: the
	// idempotency check inside Allocate is keyed on the allocation_id alone,
	// so a resend always short-circuits on the first fleet tried, regardless
	// of which fleet actually won — no special-casing needed here.
	var lastErr error
	for _, fleet := range fleets {
		res, err := s.allocator.Allocate(ctx, allocation.Request{
			AllocationID:     allocation.AllocationID(m.GetIdempotencyKey()),
			FleetID:          fleet.ID,
			SessionID:        m.GetMetadata()["sessionId"],
			Metadata:         m.GetMetadata(),
			Selectors:        selectors,
			PatchLabels:      m.GetGameServerMetadata().GetLabels(),
			PatchAnnotations: m.GetGameServerMetadata().GetAnnotations(),
			CounterFilters:   counterFilters,
			Priorities:       priorities,
			AllowAllocated:   m.GetAllowAllocated(),
		})
		if err == nil {
			return connect.NewResponse(&arenav1.AllocateResponse{
				AllocationId: res.Allocation.ID,
				GameServer:   convert.GameServerToProto(&res.GameServer),
				AllocatedAt:  res.Allocation.AllocatedAt,
			}), nil
		}
		if connect.CodeOf(err) != connect.CodeResourceExhausted {
			return nil, err // allocator already speaks connect codes
		}
		lastErr = err
	}
	return nil, lastErr // every candidate fleet exhausted
}

// resolveFleetSelector resolves fleet_selector to the ordered candidate
// fleets: every fleet in the namespace whose labels are a
// superset of the selector, oldest-created first (fleet creation order
// decides fallback priority beyond what selectors already impose). Cached
// per (namespace, selector) for fleetSelectorCacheTTL.
func (s *AllocationServer) resolveFleetSelector(ctx context.Context, namespace string, selector map[string]string) ([]store.Fleet, error) {
	key := namespace + "\x00" + selectorCacheKey(selector)
	now := s.timeNow()

	s.fleetSelectorMu.Lock()
	if e, ok := s.fleetSelectorCache[key]; ok && now.Before(e.expiresAt) {
		s.fleetSelectorMu.Unlock()
		return e.fleets, nil
	}
	s.fleetSelectorMu.Unlock()

	all, err := s.store.ListAllFleetsByNamespace(ctx, namespace)
	if err != nil {
		return nil, err
	}
	var matched []store.Fleet
	for _, f := range all {
		if fleetLabelsMatch(f.Labels, selector) {
			matched = append(matched, f)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt < matched[j].CreatedAt })

	s.fleetSelectorMu.Lock()
	if s.fleetSelectorCache == nil {
		s.fleetSelectorCache = map[string]fleetSelectorCacheEntry{}
	}
	s.fleetSelectorCache[key] = fleetSelectorCacheEntry{fleets: matched, expiresAt: now.Add(fleetSelectorCacheTTL)}
	s.fleetSelectorMu.Unlock()
	return matched, nil
}

// selectorCacheKey renders a label map into a deterministic cache key.
func selectorCacheKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte('\x00')
	}
	return b.String()
}

// fleetLabelsMatch reports whether a fleet's labels satisfy every entry of
// a fleet_selector (exact match, extra fleet labels allowed).
func fleetLabelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// selectorsFromProto converts and validates the selector fallback chain:
// match_fields keys are limited to "id"/"spec_hash", and
// every requirement needs a key and a known operator.
func selectorsFromProto(sels []*arenav1.Selectors) ([]allocation.Selector, error) {
	out := make([]allocation.Selector, 0, len(sels))
	for i, sel := range sels {
		for k := range sel.GetMatchFields() {
			if k != "id" && k != "spec_hash" {
				return nil, fmt.Errorf("selectors[%d].match_fields: unsupported key %q (want \"id\" or \"spec_hash\")", i, k)
			}
		}
		required, err := requirementsFromProto(sel.GetRequired())
		if err != nil {
			return nil, fmt.Errorf("selectors[%d].required: %w", i, err)
		}
		preferred, err := requirementsFromProto(sel.GetPreferred())
		if err != nil {
			return nil, fmt.Errorf("selectors[%d].preferred: %w", i, err)
		}
		out = append(out, allocation.Selector{
			MatchLabels: sel.GetMatchLabels(),
			MatchFields: sel.GetMatchFields(),
			Required:    required,
			Preferred:   preferred,
		})
	}
	return out, nil
}

func requirementsFromProto(reqs []*arenav1.Requirement) ([]allocation.Requirement, error) {
	var out []allocation.Requirement
	for _, r := range reqs {
		if r.GetKey() == "" {
			return nil, errors.New("requirement key is required")
		}
		var op allocation.Operator
		switch r.GetOperator() {
		case arenav1.Requirement_OPERATOR_EQUALS:
			op = allocation.OpEquals
		case arenav1.Requirement_OPERATOR_NOT_EQUALS:
			op = allocation.OpNotEquals
		case arenav1.Requirement_OPERATOR_IN:
			op = allocation.OpIn
		case arenav1.Requirement_OPERATOR_NOT_IN:
			op = allocation.OpNotIn
		case arenav1.Requirement_OPERATOR_EXISTS:
			op = allocation.OpExists
		case arenav1.Requirement_OPERATOR_NOT_EXISTS:
			op = allocation.OpNotExists
		default:
			return nil, fmt.Errorf("requirement %q: operator is required", r.GetKey())
		}
		switch op {
		case allocation.OpEquals, allocation.OpNotEquals:
			if len(r.GetValues()) != 1 {
				return nil, fmt.Errorf("requirement %q: %s needs exactly one value", r.GetKey(), op)
			}
		case allocation.OpIn, allocation.OpNotIn:
			if len(r.GetValues()) == 0 {
				return nil, fmt.Errorf("requirement %q: %s needs at least one value", r.GetKey(), op)
			}
		}
		out = append(out, allocation.Requirement{Key: r.GetKey(), Operator: op, Values: r.GetValues()})
	}
	return out, nil
}

// counterFiltersFromProto converts and validates AllocateRequest.counter_filters:
// every filter needs a name, and max_available (when set)
// must not be below min_available.
func counterFiltersFromProto(filters []*arenav1.CounterFilter) ([]allocation.CounterFilter, error) {
	out := make([]allocation.CounterFilter, 0, len(filters))
	for i, f := range filters {
		if f.GetName() == "" {
			return nil, fmt.Errorf("counter_filters[%d]: name is required", i)
		}
		min := f.GetMinAvailable()
		if min < 0 {
			return nil, fmt.Errorf("counter_filters[%d]: min_available must not be negative", i)
		}
		if max := f.GetMaxAvailable(); max < 0 {
			return nil, fmt.Errorf("counter_filters[%d]: max_available must not be negative", i)
		} else if max > 0 && max < min {
			return nil, fmt.Errorf("counter_filters[%d]: max_available must be >= min_available", i)
		}
		out = append(out, allocation.CounterFilter{
			Name: f.GetName(), MinAvailable: min, MaxAvailable: f.GetMaxAvailable(),
		})
	}
	return out, nil
}

// prioritiesFromProto converts AllocateRequest.priorities.
func prioritiesFromProto(prios []*arenav1.Priority) ([]allocation.Priority, error) {
	out := make([]allocation.Priority, 0, len(prios))
	for i, p := range prios {
		if p.GetCounter() == "" {
			return nil, fmt.Errorf("priorities[%d]: counter is required", i)
		}
		var order allocation.PriorityOrder
		switch p.GetOrder() {
		case arenav1.Priority_ORDER_ASCENDING, arenav1.Priority_ORDER_UNSPECIFIED:
			order = allocation.PriorityAscending
		case arenav1.Priority_ORDER_DESCENDING:
			order = allocation.PriorityDescending
		default:
			return nil, fmt.Errorf("priorities[%d]: unknown order", i)
		}
		out = append(out, allocation.Priority{Counter: p.GetCounter(), Order: order})
	}
	return out, nil
}

func (s *AllocationServer) Release(ctx context.Context, req *connect.Request[arenav1.ReleaseRequest]) (*connect.Response[emptypb.Empty], error) {
	if req.Msg.GetAllocationId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("allocation_id is required"))
	}
	if err := s.allocator.Release(ctx, req.Msg.GetAllocationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (s *AllocationServer) GetAllocation(ctx context.Context, req *connect.Request[arenav1.GetAllocationRequest]) (*connect.Response[arenav1.Allocation], error) {
	if req.Msg.GetAllocationId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("allocation_id is required"))
	}
	a, err := s.store.GetAllocation(ctx, req.Msg.GetAllocationId())
	if err != nil {
		return nil, asConnectError(err)
	}
	return connect.NewResponse(convert.AllocationToProto(a)), nil
}
