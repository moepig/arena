package allocation

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// fakeStore is an in-memory Store enforcing the same conditions as DynamoDB.
type fakeStore struct {
	gameservers map[string]*store.GameServer
	allocations map[string]*store.Allocation
	listCalls   int // ListAllGameServersByFleet invocations (GSI slow path)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		gameservers: map[string]*store.GameServer{},
		allocations: map[string]*store.Allocation{},
	}
}

func (f *fakeStore) GetAllocation(_ context.Context, id string) (*store.Allocation, error) {
	if a, ok := f.allocations[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetGameServer(_ context.Context, id string) (*store.GameServer, error) {
	if gs, ok := f.gameservers[id]; ok {
		cp := *gs
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ClaimGameServer(_ context.Context, gsID string, alloc store.Allocation, mutate func(*store.GameServer)) (*store.GameServer, error) {
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

func (f *fakeStore) ListAllGameServersByFleet(_ context.Context, fleetID string, state store.State) ([]store.GameServer, error) {
	f.listCalls++
	var out []store.GameServer
	for _, gs := range f.gameservers {
		if gs.FleetID == fleetID && (state == "" || gs.State == state) {
			out = append(out, *gs)
		}
	}
	return out, nil
}

func (f *fakeStore) TransitionState(_ context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != from {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	gs.State = to
	gs.Version++
	if to == store.StateReady {
		gs.ReadyAt = 777
	}
	if mutate != nil {
		mutate(gs)
	}
	cp := *gs
	return &cp, nil
}

func (f *fakeStore) ReleaseAllocation(_ context.Context, id string) error {
	if a, ok := f.allocations[id]; ok && a.ReleasedAt == 0 {
		a.ReleasedAt = 99999
	}
	return nil
}

func (f *fakeStore) AddAllocation(_ context.Context, gsID string, alloc store.Allocation) (*store.Allocation, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != store.StateAllocated {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	if _, exists := f.allocations[alloc.ID]; exists {
		return nil, fmt.Errorf("%w: allocation exists", store.ErrConditionFailed)
	}
	alloc.GameServerID = gs.ID
	alloc.FleetID = gs.FleetID
	alloc.AllocatedAt = 54321
	f.allocations[alloc.ID] = &alloc
	cp := alloc
	return &cp, nil
}

// fakePool is a slice-backed FIFO pool. Selector sub-pools are keyed by
// pool.SelectorHash, mirroring the real layout.
type fakePool struct {
	queues    map[string][]string // fleetID (or fleetID+"/"+selHash) → FIFO
	published [][]byte
	// counters: gsID -> counter name -> state.
	counters map[string]map[string]pool.Counter
}

func newFakePool() *fakePool {
	return &fakePool{queues: map[string][]string{}, counters: map[string]map[string]pool.Counter{}}
}

func (f *fakePool) pop(key string) (string, error) {
	q := f.queues[key]
	if len(q) == 0 {
		return "", pool.ErrEmpty
	}
	f.queues[key] = q[1:]
	return q[0], nil
}

func (f *fakePool) PopMin(_ context.Context, fleetID string) (string, error) {
	return f.pop(fleetID)
}

func (f *fakePool) PopMinSelector(_ context.Context, fleetID string, labels map[string]string) (string, error) {
	h := pool.SelectorHash(labels)
	if h == "" {
		return "", pool.ErrEmpty
	}
	return f.pop(fleetID + "/" + h)
}

func (f *fakePool) Add(_ context.Context, fleetID, gsID string, _ float64, labels map[string]string) error {
	f.queues[fleetID] = append(f.queues[fleetID], gsID)
	if h := pool.SelectorHash(labels); h != "" {
		f.queues[fleetID+"/"+h] = append(f.queues[fleetID+"/"+h], gsID)
	}
	return nil
}

func (f *fakePool) Remove(_ context.Context, fleetID, gsID string) error {
	q := f.queues[fleetID]
	for i, id := range q {
		if id == gsID {
			f.queues[fleetID] = append(q[:i], q[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakePool) PublishAllocation(_ context.Context, _ string, payload []byte) error {
	f.published = append(f.published, payload)
	return nil
}

func (f *fakePool) Counters(_ context.Context, ids []string) (map[string]pool.Snapshot, error) {
	out := map[string]pool.Snapshot{}
	for _, id := range ids {
		c, ok := f.counters[id]
		if !ok {
			continue
		}
		cp := make(map[string]pool.Counter, len(c))
		for k, v := range c {
			cp[k] = v
		}
		out[id] = pool.Snapshot{Counters: cp}
	}
	return out, nil
}

func (f *fakePool) ReserveCounter(_ context.Context, _, name, gsID string, amount int64) (bool, error) {
	c, ok := f.counters[gsID]
	if !ok {
		return false, nil
	}
	cnt, ok := c[name]
	if !ok || cnt.Capacity-cnt.Count < amount {
		return false, nil
	}
	cnt.Count += amount
	c[name] = cnt
	return true, nil
}

func (f *fakePool) ReleaseCounter(_ context.Context, _, name, gsID string, amount int64) error {
	c, ok := f.counters[gsID]
	if !ok {
		return nil
	}
	cnt := c[name]
	cnt.Count -= amount
	c[name] = cnt
	return nil
}

func readyGS(id, fleetID string, labels map[string]string) *store.GameServer {
	return &store.GameServer{
		ID: id, FleetID: fleetID, State: store.StateReady,
		Labels: labels, Version: 1, ReadyAt: 100,
	}
}

func TestAllocationIDDeterministic(t *testing.T) {
	a := AllocationID("match-1")
	b := AllocationID("match-1")
	c := AllocationID("match-2")
	if a != b {
		t.Errorf("same key produced different IDs: %s vs %s", a, b)
	}
	if a == c {
		t.Error("different keys produced the same ID")
	}
}

func TestAllocateFastPath(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", nil)
	fp.queues["f1"] = []string{"gs-1"}
	a := New(fs, fp, func(*store.GameServer, *store.Allocation) []byte { return []byte("push") })

	res, err := a.Allocate(context.Background(), Request{AllocationID: AllocationID("k1"), FleetID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-1" || res.GameServer.State != store.StateAllocated {
		t.Fatalf("unexpected result: %+v", res.GameServer)
	}
	if len(fp.published) != 1 {
		t.Errorf("expected 1 sidecar push, got %d", len(fp.published))
	}
}

func TestAllocateIdempotentResend(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", nil)
	fs.gameservers["gs-2"] = readyGS("gs-2", "f1", nil)
	fp.queues["f1"] = []string{"gs-1", "gs-2"}
	a := New(fs, fp, nil)

	req := Request{AllocationID: AllocationID("k1"), FleetID: "f1"}
	first, err := a.Allocate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Allocate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.GameServer.ID != first.GameServer.ID {
		t.Fatalf("resend allocated a different server: %s vs %s", second.GameServer.ID, first.GameServer.ID)
	}
	// gs-2 must still be available for other keys.
	if len(fp.queues["f1"]) != 1 {
		t.Errorf("resend consumed pool inventory: %v", fp.queues["f1"])
	}
}

func TestAllocateSkipsStaleCandidates(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	// gs-1 is pooled but already Unhealthy (race with health reconciler).
	unhealthy := readyGS("gs-1", "f1", nil)
	unhealthy.State = store.StateUnhealthy
	fs.gameservers["gs-1"] = unhealthy
	fs.gameservers["gs-2"] = readyGS("gs-2", "f1", nil)
	fp.queues["f1"] = []string{"gs-1", "gs-2"}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{AllocationID: AllocationID("k1"), FleetID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-2" {
		t.Fatalf("allocated %s, want gs-2", res.GameServer.ID)
	}
}

func TestAllocateExhausted(t *testing.T) {
	a := New(newFakeStore(), newFakePool(), nil)
	_, err := a.Allocate(context.Background(), Request{AllocationID: AllocationID("k1"), FleetID: "f1"})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestAllocateSelectorPath(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-a"] = readyGS("gs-a", "f1", map[string]string{"version": "v1"})
	fs.gameservers["gs-b"] = readyGS("gs-b", "f1", map[string]string{"version": "v2"})
	fp.queues["f1"] = []string{"gs-a", "gs-b"}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{MatchLabels: map[string]string{"version": "v2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-b" {
		t.Fatalf("allocated %s, want gs-b", res.GameServer.ID)
	}
	// Winner must be removed from the pool; the other stays.
	if len(fp.queues["f1"]) != 1 || fp.queues["f1"][0] != "gs-a" {
		t.Errorf("pool after selector claim = %v, want [gs-a]", fp.queues["f1"])
	}

	_, err = a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k2"), FleetID: "f1",
		Selectors: []Selector{{MatchLabels: map[string]string{"version": "v9"}}},
	})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("no-match err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestReleaseReturnsServerToPool(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", nil)
	fp.queues["f1"] = []string{"gs-1"}
	a := New(fs, fp, nil)

	allocID := AllocationID("k1")
	if _, err := a.Allocate(context.Background(), Request{AllocationID: allocID, FleetID: "f1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background(), allocID); err != nil {
		t.Fatal(err)
	}
	if fs.gameservers["gs-1"].State != store.StateReady {
		t.Fatalf("state = %s, want Ready", fs.gameservers["gs-1"].State)
	}
	if len(fp.queues["f1"]) != 1 {
		t.Fatalf("server not returned to pool: %v", fp.queues["f1"])
	}
	// Releasing twice is a no-op (server already back to Ready).
	if err := a.Release(context.Background(), allocID); err != nil {
		t.Fatalf("double release: %v", err)
	}
	if len(fp.queues["f1"]) != 1 {
		t.Fatalf("double release duplicated pool entry: %v", fp.queues["f1"])
	}
}

func TestReleaseUnknownAllocation(t *testing.T) {
	a := New(newFakeStore(), newFakePool(), nil)
	err := a.Release(context.Background(), "nope")
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
}
