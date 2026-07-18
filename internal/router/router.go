// Package router implements the allocation router: the
// Agones GameServerAllocationPolicy equivalent for arena's per-region
// deployment model. arena has no cross-region SoT (DynamoDB Global Tables
// are deliberately out of the allocation hot path), so instead of a
// cluster-aware allocator service, the router is a stateless thin layer
// that forwards AllocationService calls to statically configured regional
// arena-api endpoints, trying them in priority order (weighted-random
// within a priority tier) and falling back to the next region on
// RESOURCE_EXHAUSTED. Existing per-region IAM auth applies unchanged across
// regions — no separate mTLS trust fabric is needed.
package router

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// Region is one entry of the static routing policy: [{region, endpoint,
// priority, weight}].
type Region struct {
	Name     string
	Endpoint string
	// Priority groups are tried lowest-value-first; a group is exhausted
	// (falls through to the next) only when every region in it returns
	// RESOURCE_EXHAUSTED (Allocate) or NOT_FOUND (Release/GetAllocation).
	Priority int32
	// Weight biases try-order within a priority group: higher weight is
	// more likely to be tried first (and more often, across many calls).
	// <= 0 is still selectable, just heavily deprioritized — never starved.
	Weight int32
	Client arenav1connect.AllocationServiceClient
}

// Router forwards arena.v1.AllocationService calls across the configured
// regions. The zero value is not usable; construct with New.
type Router struct {
	groups [][]Region // outer: priority ascending
	next   func() float64
}

// New groups regions by priority (ascending) and returns a Router. next
// yields values in [0,1); nil defaults to math/rand/v2 — tests inject a
// deterministic source for reproducible ordering.
func New(regions []Region, next func() float64) *Router {
	byPriority := map[int32][]Region{}
	for _, r := range regions {
		byPriority[r.Priority] = append(byPriority[r.Priority], r)
	}
	priorities := make([]int32, 0, len(byPriority))
	for p := range byPriority {
		priorities = append(priorities, p)
	}
	sort.Slice(priorities, func(i, j int) bool { return priorities[i] < priorities[j] })
	groups := make([][]Region, len(priorities))
	for i, p := range priorities {
		groups[i] = byPriority[p]
	}
	if next == nil {
		next = rand.Float64
	}
	return &Router{groups: groups, next: next}
}

// weightOf floors a region's effective weight so it always has a (small)
// chance of being tried, even at weight 0 — the routing policy shouldn't be
// able to starve a configured region outright.
func weightOf(r Region) float64 {
	if r.Weight <= 0 {
		return 0.01
	}
	return float64(r.Weight)
}

// weightedOrder returns regions in a weighted-random order without
// replacement (roulette-wheel selection): higher weight is more likely to
// land earlier, but every region eventually gets a turn.
func weightedOrder(regions []Region, next func() float64) []Region {
	remaining := append([]Region(nil), regions...)
	order := make([]Region, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0.0
		for _, r := range remaining {
			total += weightOf(r)
		}
		target := next() * total
		acc, pick := 0.0, len(remaining)-1
		for i, r := range remaining {
			acc += weightOf(r)
			if target < acc {
				pick = i
				break
			}
		}
		order = append(order, remaining[pick])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return order
}

var errNoRegions = errors.New("router: no region configured")

// Allocate tries each configured region in priority/weight order, falling
// back to the next on RESOURCE_EXHAUSTED. Any other error aborts
// immediately (it isn't "this region is full", it's broken or the request
// is invalid, and retrying elsewhere won't help). Returns the name of the
// region that served the request.
func (r *Router) Allocate(ctx context.Context, req *arenav1.AllocateRequest) (*arenav1.AllocateResponse, string, error) {
	lastErr := error(connect.NewError(connect.CodeResourceExhausted, errNoRegions))
	for _, group := range r.groups {
		for _, region := range weightedOrder(group, r.next) {
			res, err := region.Client.Allocate(ctx, connect.NewRequest(req))
			if err == nil {
				return res.Msg, region.Name, nil
			}
			if connect.CodeOf(err) != connect.CodeResourceExhausted {
				return nil, region.Name, err
			}
			lastErr = err
		}
	}
	return nil, "", lastErr
}

// Release fans the release out across regions in priority/weight order
// until one succeeds — the router holds no record of which region served
// the original Allocate. NOT_FOUND is the "keep looking" signal here (the
// allocation simply doesn't live in that region); any other error aborts.
func (r *Router) Release(ctx context.Context, req *arenav1.ReleaseRequest) error {
	lastErr := error(connect.NewError(connect.CodeNotFound, errNoRegions))
	for _, group := range r.groups {
		for _, region := range weightedOrder(group, r.next) {
			_, err := region.Client.Release(ctx, connect.NewRequest(req))
			if err == nil {
				return nil
			}
			if connect.CodeOf(err) != connect.CodeNotFound {
				return err
			}
			lastErr = err
		}
	}
	return lastErr
}

// GetAllocation fans out the same way Release does.
func (r *Router) GetAllocation(ctx context.Context, req *arenav1.GetAllocationRequest) (*arenav1.Allocation, error) {
	lastErr := error(connect.NewError(connect.CodeNotFound, errNoRegions))
	for _, group := range r.groups {
		for _, region := range weightedOrder(group, r.next) {
			res, err := region.Client.GetAllocation(ctx, connect.NewRequest(req))
			if err == nil {
				return res.Msg, nil
			}
			if connect.CodeOf(err) != connect.CodeNotFound {
				return nil, err
			}
			lastErr = err
		}
	}
	return nil, lastErr
}

// Handler adapts a Router to arenav1connect.AllocationServiceHandler so it
// can be mounted directly as the arena-router service.
type Handler struct {
	arenav1connect.UnimplementedAllocationServiceHandler
	Router *Router
}

func (h *Handler) Allocate(ctx context.Context, req *connect.Request[arenav1.AllocateRequest]) (*connect.Response[arenav1.AllocateResponse], error) {
	res, _, err := h.Router.Allocate(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

func (h *Handler) Release(ctx context.Context, req *connect.Request[arenav1.ReleaseRequest]) (*connect.Response[emptypb.Empty], error) {
	if err := h.Router.Release(ctx, req.Msg); err != nil {
		return nil, err
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (h *Handler) GetAllocation(ctx context.Context, req *connect.Request[arenav1.GetAllocationRequest]) (*connect.Response[arenav1.Allocation], error) {
	res, err := h.Router.GetAllocation(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

var _ arenav1connect.AllocationServiceHandler = (*Handler)(nil)
