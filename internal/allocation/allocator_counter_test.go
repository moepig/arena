package allocation

// Counter filters, priorities, and the high-density
// reallocation path (allow_allocated).

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

func TestCounterFilterExcludesFullServersAndMissingSnapshots(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-full"] = readyGS("gs-full", "f1", nil)
	fs.gameservers["gs-open"] = readyGS("gs-open", "f1", nil)
	fs.gameservers["gs-unknown"] = readyGS("gs-unknown", "f1", nil) // never reported counters
	fp.counters["gs-full"] = map[string]pool.Counter{"rooms": {Count: 4, Capacity: 4}}
	fp.counters["gs-open"] = map[string]pool.Counter{"rooms": {Count: 1, Capacity: 4}}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-open" {
		t.Fatalf("allocated %s, want gs-open (gs-full has no available capacity, gs-unknown has no snapshot)", res.GameServer.ID)
	}
}

func TestCounterFilterMinAvailableDefaultsToOne(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-empty"] = readyGS("gs-empty", "f1", nil)
	fp.counters["gs-empty"] = map[string]pool.Counter{"rooms": {Count: 0, Capacity: 0}} // available=0
	a := New(fs, fp, nil)

	_, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms"}}, // MinAvailable omitted → defaults to 1
	})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED (available=0 fails the implicit min=1)", err)
	}
}

func TestCounterFilterMaxAvailableUnboundedWhenZero(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", nil)
	fp.counters["gs-1"] = map[string]pool.Counter{"rooms": {Count: 0, Capacity: 100}} // available=100
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1, MaxAvailable: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-1" {
		t.Fatalf("allocated %s, want gs-1", res.GameServer.ID)
	}
}

func TestPriorityAscendingPacksTightest(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-loose"] = readyGS("gs-loose", "f1", nil)
	fs.gameservers["gs-tight"] = readyGS("gs-tight", "f1", nil)
	fp.counters["gs-loose"] = map[string]pool.Counter{"rooms": {Count: 1, Capacity: 10}} // available=9
	fp.counters["gs-tight"] = map[string]pool.Counter{"rooms": {Count: 8, Capacity: 10}} // available=2
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		Priorities:     []Priority{{Counter: "rooms", Order: PriorityAscending}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-tight" {
		t.Fatalf("allocated %s, want gs-tight (ascending = least available first, packs tightest)", res.GameServer.ID)
	}
}

func TestPriorityDescendingSpreads(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-loose"] = readyGS("gs-loose", "f1", nil)
	fs.gameservers["gs-tight"] = readyGS("gs-tight", "f1", nil)
	fp.counters["gs-loose"] = map[string]pool.Counter{"rooms": {Count: 1, Capacity: 10}} // available=9
	fp.counters["gs-tight"] = map[string]pool.Counter{"rooms": {Count: 8, Capacity: 10}} // available=2
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		Priorities:     []Priority{{Counter: "rooms", Order: PriorityDescending}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-loose" {
		t.Fatalf("allocated %s, want gs-loose (descending = most available first, spreads load)", res.GameServer.ID)
	}
}

func TestAllowAllocatedHighDensityReallocation(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	allocated := readyGS("gs-1", "f1", nil)
	allocated.State = store.StateAllocated
	fs.gameservers["gs-1"] = allocated
	fs.allocations["existing"] = &store.Allocation{ID: "existing", GameServerID: "gs-1", FleetID: "f1"}
	fp.counters["gs-1"] = map[string]pool.Counter{"rooms": {Count: 1, Capacity: 4}} // available=3
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		AllowAllocated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-1" || res.GameServer.State != store.StateAllocated {
		t.Fatalf("result = %+v, want gs-1 staying Allocated", res.GameServer)
	}
	if len(fs.allocations) != 2 {
		t.Fatalf("allocations = %d, want 2 (existing + new, both live on gs-1)", len(fs.allocations))
	}
	got := fp.counters["gs-1"]["rooms"]
	if got.Count != 2 {
		t.Errorf("rooms count = %d, want 2 (reservation consumed 1 of the 3 available)", got.Count)
	}
}

func TestAllowAllocatedRequiresCounterFilterToConsiderAllocated(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	// Allocated server that would be eligible by state alone, but no
	// counter_filters means allocateCounter is never entered — Priorities
	// alone still routes here, so use Priorities without AllowAllocated to
	// confirm Ready-only candidates are considered when the flag is unset.
	allocated := readyGS("gs-1", "f1", nil)
	allocated.State = store.StateAllocated
	fs.gameservers["gs-1"] = allocated
	fp.counters["gs-1"] = map[string]pool.Counter{"rooms": {Count: 0, Capacity: 4}}
	a := New(fs, fp, nil)

	_, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		AllowAllocated: false,
	})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED (Allocated servers excluded without allow_allocated)", err)
	}
}

func TestAllowAllocatedReservationRaceFallsThroughToNextCandidate(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	tight := readyGS("gs-tight", "f1", nil)
	tight.State = store.StateAllocated
	fs.gameservers["gs-tight"] = tight
	loose := readyGS("gs-loose", "f1", nil)
	loose.State = store.StateAllocated
	fs.gameservers["gs-loose"] = loose
	// gs-tight ranks first (ascending, least available) but its capacity is
	// exhausted between snapshot read and reservation (simulated directly).
	fp.counters["gs-tight"] = map[string]pool.Counter{"rooms": {Count: 4, Capacity: 4}} // available=0, would fail the filter anyway
	fp.counters["gs-loose"] = map[string]pool.Counter{"rooms": {Count: 0, Capacity: 4}} // available=4
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   AllocationID("k1"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		Priorities:     []Priority{{Counter: "rooms", Order: PriorityAscending}},
		AllowAllocated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-loose" {
		t.Fatalf("allocated %s, want gs-loose (gs-tight fails the min_available filter)", res.GameServer.ID)
	}
}

func TestReleaseAdditionalAllocationRestoresCounterAndNeverReadies(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	allocated := readyGS("gs-1", "f1", nil)
	allocated.State = store.StateAllocated
	fs.gameservers["gs-1"] = allocated
	fp.counters["gs-1"] = map[string]pool.Counter{"rooms": {Count: 1, Capacity: 4}} // available=3
	a := New(fs, fp, nil)

	allocID := AllocationID("k1")
	res, err := a.Allocate(context.Background(), Request{
		AllocationID:   allocID,
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		AllowAllocated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fp.counters["gs-1"]["rooms"]; got.Count != 2 {
		t.Fatalf("rooms count after allocate = %d, want 2 (1 reserved)", got.Count)
	}

	if err := a.Release(context.Background(), res.Allocation.ID); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateAllocated {
		t.Fatalf("state after releasing an additional allocation = %s, want Allocated (unchanged)", got)
	}
	if got := fp.counters["gs-1"]["rooms"]; got.Count != 1 {
		t.Fatalf("rooms count after release = %d, want 1 (reservation restored)", got.Count)
	}
	if len(fp.queues["f1"]) != 0 {
		t.Errorf("pool = %v, want the server not returned to the ready pool", fp.queues["f1"])
	}
}

func TestAllowAllocatedIdempotentResendReturnsExistingAllocation(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	allocated := readyGS("gs-1", "f1", nil)
	allocated.State = store.StateAllocated
	fs.gameservers["gs-1"] = allocated
	fp.counters["gs-1"] = map[string]pool.Counter{"rooms": {Count: 0, Capacity: 4}}
	a := New(fs, fp, nil)

	req := Request{
		AllocationID:   AllocationID("same-key"),
		FleetID:        "f1",
		CounterFilters: []CounterFilter{{Name: "rooms", MinAvailable: 1}},
		AllowAllocated: true,
	}
	first, err := a.Allocate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Allocate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Allocation.ID != first.Allocation.ID || !second.Reused {
		t.Fatalf("resend = %+v, want the same allocation marked Reused", second)
	}
	if len(fs.allocations) != 1 {
		t.Fatalf("allocations = %d, want 1 (resend must not create a duplicate)", len(fs.allocations))
	}
}
