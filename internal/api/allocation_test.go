package api

// Cross-fleet allocation via fleet_selector — resolution,
// creation-order fallback, the 8-fleet cap, and the 60s cache.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// fakeAllocStore backs both api.AllocationStore and allocation.Store with a
// single in-memory implementation.
type fakeAllocStore struct {
	fleets      map[string]*store.Fleet
	gameservers map[string]*store.GameServer
	allocations map[string]*store.Allocation

	listFleetsCalls int
}

func newFakeAllocStore() *fakeAllocStore {
	return &fakeAllocStore{
		fleets:      map[string]*store.Fleet{},
		gameservers: map[string]*store.GameServer{},
		allocations: map[string]*store.Allocation{},
	}
}

func (f *fakeAllocStore) GetFleetByName(_ context.Context, namespace, name string) (*store.Fleet, error) {
	for _, fl := range f.fleets {
		if fl.Namespace == namespace && fl.Name == name {
			cp := *fl
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeAllocStore) ListAllFleetsByNamespace(_ context.Context, namespace string) ([]store.Fleet, error) {
	f.listFleetsCalls++
	var out []store.Fleet
	for _, fl := range f.fleets {
		if fl.Namespace == namespace {
			out = append(out, *fl)
		}
	}
	return out, nil
}

func (f *fakeAllocStore) GetAllocation(_ context.Context, id string) (*store.Allocation, error) {
	if a, ok := f.allocations[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAllocStore) GetGameServer(_ context.Context, id string) (*store.GameServer, error) {
	if gs, ok := f.gameservers[id]; ok {
		cp := *gs
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeAllocStore) ClaimGameServer(_ context.Context, gsID string, alloc store.Allocation, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != store.StateReady {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	if _, exists := f.allocations[alloc.ID]; exists {
		return nil, fmt.Errorf("%w: allocation exists", store.ErrConditionFailed)
	}
	gs.State = store.StateAllocated
	gs.AllocatedAt = 12345
	gs.Version++
	if mutate != nil {
		mutate(gs)
	}
	alloc.GameServerID = gs.ID
	alloc.FleetID = gs.FleetID
	alloc.AllocatedAt = gs.AllocatedAt
	f.allocations[alloc.ID] = &alloc
	cp := *gs
	return &cp, nil
}

func (f *fakeAllocStore) ListAllGameServersByFleet(_ context.Context, fleetID string, state store.State) ([]store.GameServer, error) {
	var out []store.GameServer
	for _, gs := range f.gameservers {
		if gs.FleetID == fleetID && (state == "" || gs.State == state) {
			out = append(out, *gs)
		}
	}
	return out, nil
}

func (f *fakeAllocStore) TransitionState(_ context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != from {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	gs.State = to
	gs.Version++
	if mutate != nil {
		mutate(gs)
	}
	cp := *gs
	return &cp, nil
}

func (f *fakeAllocStore) ReleaseAllocation(_ context.Context, id string) error {
	if a, ok := f.allocations[id]; ok {
		a.ReleasedAt = 99999
	}
	return nil
}

func (f *fakeAllocStore) AddAllocation(_ context.Context, gsID string, alloc store.Allocation) (*store.Allocation, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != store.StateAllocated {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	alloc.GameServerID = gsID
	alloc.FleetID = gs.FleetID
	f.allocations[alloc.ID] = &alloc
	cp := alloc
	return &cp, nil
}

// fakeAllocPool is a slice-backed FIFO pool sufficient for the fast path.
type fakeAllocPool struct {
	queues map[string][]string
}

func newFakeAllocPool() *fakeAllocPool { return &fakeAllocPool{queues: map[string][]string{}} }

func (p *fakeAllocPool) PopMin(_ context.Context, fleetID string) (string, error) {
	q := p.queues[fleetID]
	if len(q) == 0 {
		return "", pool.ErrEmpty
	}
	p.queues[fleetID] = q[1:]
	return q[0], nil
}
func (p *fakeAllocPool) PopMinSelector(context.Context, string, map[string]string) (string, error) {
	return "", pool.ErrEmpty
}
func (p *fakeAllocPool) Add(_ context.Context, fleetID, gsID string, _ float64, _ map[string]string) error {
	p.queues[fleetID] = append(p.queues[fleetID], gsID)
	return nil
}
func (p *fakeAllocPool) Remove(_ context.Context, fleetID, gsID string) error {
	q := p.queues[fleetID]
	for i, id := range q {
		if id == gsID {
			p.queues[fleetID] = append(q[:i], q[i+1:]...)
			break
		}
	}
	return nil
}
func (p *fakeAllocPool) PublishAllocation(context.Context, string, []byte) error { return nil }
func (p *fakeAllocPool) Counters(context.Context, []string) (map[string]pool.Snapshot, error) {
	return nil, nil
}
func (p *fakeAllocPool) ReserveCounter(context.Context, string, string, string, int64) (bool, error) {
	return false, nil
}
func (p *fakeAllocPool) ReleaseCounter(context.Context, string, string, string, int64) error {
	return nil
}

func newAllocFleet(id, namespace, name string, labels map[string]string, createdAt int64) *store.Fleet {
	return &store.Fleet{ID: id, Namespace: namespace, Name: name, Labels: labels, CreatedAt: createdAt, Version: 1}
}

func newAllocReadyGS(id, fleetID string) *store.GameServer {
	return &store.GameServer{ID: id, FleetID: fleetID, State: store.StateReady, Version: 1}
}

func TestAllocateRequiresExactlyOneOfNameOrSelector(t *testing.T) {
	fs := newFakeAllocStore()
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, newFakeAllocPool(), nil)}

	_, err := srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{IdempotencyKey: "k1"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("neither set: err = %v, want INVALID_ARGUMENT", err)
	}

	_, err = srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{
		IdempotencyKey: "k1", FleetName: "f1", FleetSelector: map[string]string{"a": "b"},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("both set: err = %v, want INVALID_ARGUMENT", err)
	}
}

func TestAllocateFleetSelectorTriesFleetsInCreationOrder(t *testing.T) {
	fs := newFakeAllocStore()
	labels := map[string]string{"game": "arena"}
	fs.fleets["f-old"] = newAllocFleet("f-old", "default", "old", labels, 100)
	fs.fleets["f-new"] = newAllocFleet("f-new", "default", "new", labels, 200)
	fs.gameservers["gs-old"] = newAllocReadyGS("gs-old", "f-old")
	fs.gameservers["gs-new"] = newAllocReadyGS("gs-new", "f-new")
	pl := newFakeAllocPool()
	pl.queues["f-old"] = []string{"gs-old"}
	pl.queues["f-new"] = []string{"gs-new"}
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, pl, nil)}

	res, err := srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{
		IdempotencyKey: "k1", FleetSelector: labels,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetGameServer().GetId() != "gs-old" {
		t.Fatalf("allocated %s, want gs-old (oldest fleet tried first)", res.Msg.GetGameServer().GetId())
	}
}

func TestAllocateFleetSelectorFallsThroughOnResourceExhausted(t *testing.T) {
	fs := newFakeAllocStore()
	labels := map[string]string{"game": "arena"}
	fs.fleets["f-empty"] = newAllocFleet("f-empty", "default", "empty", labels, 100)
	fs.fleets["f-has"] = newAllocFleet("f-has", "default", "has", labels, 200)
	fs.gameservers["gs-1"] = newAllocReadyGS("gs-1", "f-has")
	pl := newFakeAllocPool()
	pl.queues["f-has"] = []string{"gs-1"} // f-empty has no pooled candidates
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, pl, nil)}

	res, err := srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{
		IdempotencyKey: "k1", FleetSelector: labels,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetGameServer().GetId() != "gs-1" {
		t.Fatalf("allocated %s, want gs-1 (fell through from the exhausted fleet)", res.Msg.GetGameServer().GetId())
	}
}

func TestAllocateFleetSelectorNoMatchIsResourceExhausted(t *testing.T) {
	fs := newFakeAllocStore()
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, newFakeAllocPool(), nil)}

	_, err := srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{
		IdempotencyKey: "k1", FleetSelector: map[string]string{"a": "b"},
	}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestAllocateFleetSelectorTooManyFleetsRejected(t *testing.T) {
	fs := newFakeAllocStore()
	labels := map[string]string{"game": "arena"}
	for i := 0; i < maxFleetSelectorFleets+1; i++ {
		id := fmt.Sprintf("f-%d", i)
		fs.fleets[id] = newAllocFleet(id, "default", id, labels, int64(i))
	}
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, newFakeAllocPool(), nil)}

	_, err := srv.Allocate(context.Background(), connect.NewRequest(&arenav1.AllocateRequest{
		IdempotencyKey: "k1", FleetSelector: labels,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT (%d fleets > cap of %d)", err, maxFleetSelectorFleets+1, maxFleetSelectorFleets)
	}
}

func TestFleetSelectorResolutionIsCached(t *testing.T) {
	fs := newFakeAllocStore()
	labels := map[string]string{"game": "arena"}
	fs.fleets["f1"] = newAllocFleet("f1", "default", "f1", labels, 100)
	now := time.Unix(1_000_000, 0)
	srv := &AllocationServer{store: fs, allocator: allocation.New(fs, newFakeAllocPool(), nil), now: func() time.Time { return now }}

	if _, err := srv.resolveFleetSelector(context.Background(), "default", labels); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.resolveFleetSelector(context.Background(), "default", labels); err != nil {
		t.Fatal(err)
	}
	if fs.listFleetsCalls != 1 {
		t.Fatalf("ListAllFleetsByNamespace calls = %d, want 1 (second call served from cache)", fs.listFleetsCalls)
	}

	now = now.Add(fleetSelectorCacheTTL + time.Second)
	if _, err := srv.resolveFleetSelector(context.Background(), "default", labels); err != nil {
		t.Fatal(err)
	}
	if fs.listFleetsCalls != 2 {
		t.Fatalf("ListAllFleetsByNamespace calls = %d, want 2 (cache expired)", fs.listFleetsCalls)
	}
}
