package router

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// fakeClient is a minimal arenav1connect.AllocationServiceClient stub.
type fakeClient struct {
	name          string
	allocateErr   error
	allocateCalls *[]string
	releaseErr    error
	getErr        error
}

var _ arenav1connect.AllocationServiceClient = (*fakeClient)(nil)

func (c *fakeClient) Allocate(context.Context, *connect.Request[arenav1.AllocateRequest]) (*connect.Response[arenav1.AllocateResponse], error) {
	if c.allocateCalls != nil {
		*c.allocateCalls = append(*c.allocateCalls, c.name)
	}
	if c.allocateErr != nil {
		return nil, c.allocateErr
	}
	return connect.NewResponse(&arenav1.AllocateResponse{AllocationId: "alloc-" + c.name}), nil
}

func (c *fakeClient) Release(context.Context, *connect.Request[arenav1.ReleaseRequest]) (*connect.Response[emptypb.Empty], error) {
	if c.releaseErr != nil {
		return nil, c.releaseErr
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (c *fakeClient) GetAllocation(context.Context, *connect.Request[arenav1.GetAllocationRequest]) (*connect.Response[arenav1.Allocation], error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return connect.NewResponse(&arenav1.Allocation{AllocationId: "alloc-" + c.name}), nil
}

func exhausted() error { return connect.NewError(connect.CodeResourceExhausted, errors.New("full")) }
func notFound() error  { return connect.NewError(connect.CodeNotFound, errors.New("nope")) }

// sequence returns a next() func that replays fixed values, then repeats
// the last one (weightedOrder never asks for more than len(regions) draws
// per call, but a test may invoke Allocate more than once).
func sequence(vals ...float64) func() float64 {
	i := 0
	return func() float64 {
		v := vals[min(i, len(vals)-1)]
		i++
		return v
	}
}

func TestAllocateTriesLowerPriorityFirst(t *testing.T) {
	primary := &fakeClient{name: "us-east"}
	secondary := &fakeClient{name: "us-west"}
	r := New([]Region{
		{Name: "us-west", Priority: 2, Weight: 1, Client: secondary},
		{Name: "us-east", Priority: 1, Weight: 1, Client: primary},
	}, sequence(0))

	res, region, err := r.Allocate(context.Background(), &arenav1.AllocateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if region != "us-east" || res.GetAllocationId() != "alloc-us-east" {
		t.Fatalf("region = %s, res = %+v, want us-east (lower priority value tried first)", region, res)
	}
}

func TestAllocateFallsBackOnResourceExhausted(t *testing.T) {
	full := &fakeClient{name: "us-east", allocateErr: exhausted()}
	open := &fakeClient{name: "us-west"}
	r := New([]Region{
		{Name: "us-east", Priority: 1, Weight: 1, Client: full},
		{Name: "us-west", Priority: 2, Weight: 1, Client: open},
	}, sequence(0))

	res, region, err := r.Allocate(context.Background(), &arenav1.AllocateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if region != "us-west" || res.GetAllocationId() != "alloc-us-west" {
		t.Fatalf("region = %s, res = %+v, want us-west (fell back from the exhausted region)", region, res)
	}
}

func TestAllocateNonExhaustedErrorAbortsImmediately(t *testing.T) {
	broken := &fakeClient{name: "us-east", allocateErr: connect.NewError(connect.CodeUnavailable, errors.New("down"))}
	open := &fakeClient{name: "us-west"}
	r := New([]Region{
		{Name: "us-east", Priority: 1, Weight: 1, Client: broken},
		{Name: "us-west", Priority: 2, Weight: 1, Client: open},
	}, sequence(0))

	_, _, err := r.Allocate(context.Background(), &arenav1.AllocateRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want UNAVAILABLE to propagate without trying us-west", err)
	}
}

func TestAllocateAllExhaustedReturnsResourceExhausted(t *testing.T) {
	a := &fakeClient{name: "a", allocateErr: exhausted()}
	b := &fakeClient{name: "b", allocateErr: exhausted()}
	r := New([]Region{
		{Name: "a", Priority: 1, Weight: 1, Client: a},
		{Name: "b", Priority: 1, Weight: 1, Client: b},
	}, sequence(0, 0.99))

	_, _, err := r.Allocate(context.Background(), &arenav1.AllocateRequest{})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestWeightedOrderWithinPriorityGroup(t *testing.T) {
	var calls []string
	light := &fakeClient{name: "light", allocateErr: exhausted(), allocateCalls: &calls}
	heavy := &fakeClient{name: "heavy", allocateErr: exhausted(), allocateCalls: &calls}
	// Region order lists light first, so picking heavy first can only come
	// from its weight (3 vs 1) dominating the roulette draw, not array order.
	r := New([]Region{
		{Name: "light", Priority: 1, Weight: 1, Client: light},
		{Name: "heavy", Priority: 1, Weight: 3, Client: heavy},
	}, sequence(0.9, 0.5)) // total weight 4: target 3.6 falls in heavy's [1,4) bucket

	if _, _, err := r.Allocate(context.Background(), &arenav1.AllocateRequest{}); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "heavy" {
		t.Fatalf("call order = %v, want heavy first (weight 3 vs 1 wins the roulette draw)", calls)
	}
}

func TestReleaseFansOutOnNotFound(t *testing.T) {
	miss := &fakeClient{name: "a", releaseErr: notFound()}
	hit := &fakeClient{name: "b"}
	r := New([]Region{
		{Name: "a", Priority: 1, Weight: 1, Client: miss},
		{Name: "b", Priority: 2, Weight: 1, Client: hit},
	}, sequence(0))

	if err := r.Release(context.Background(), &arenav1.ReleaseRequest{AllocationId: "x"}); err != nil {
		t.Fatalf("err = %v, want nil (found in region b)", err)
	}
}

func TestGetAllocationFansOutOnNotFound(t *testing.T) {
	miss := &fakeClient{name: "a", getErr: notFound()}
	hit := &fakeClient{name: "b"}
	r := New([]Region{
		{Name: "a", Priority: 1, Weight: 1, Client: miss},
		{Name: "b", Priority: 2, Weight: 1, Client: hit},
	}, sequence(0))

	res, err := r.GetAllocation(context.Background(), &arenav1.GetAllocationRequest{AllocationId: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetAllocationId() != "alloc-b" {
		t.Fatalf("got %+v, want the region-b record", res)
	}
}
