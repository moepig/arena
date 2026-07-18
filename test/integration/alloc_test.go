//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/store"
)

// seedReady puts a GameServer in Ready and into the real Valkey pool.
func seedReady(t *testing.T, st *store.Store, p allocation.Pool, gsID, fleetID string) {
	t.Helper()
	gs := mustPutGS(t, st, gsID, fleetID, store.StateReady)
	if err := p.Add(context.Background(), fleetID, gsID, float64(gs.ReadyAt), nil); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentAllocateSameIdempotencyKey: parallel resends of one key
// converge on one allocation and consume one server.
func TestConcurrentAllocateSameIdempotencyKey(t *testing.T) {
	st, p := newStore(t), newPool(t)
	fleetID := "f-idem-" + t.Name()
	seedReady(t, st, p, "gs-idem-1", fleetID)
	seedReady(t, st, p, "gs-idem-2", fleetID)
	a := allocation.New(st, p, nil)

	req := allocation.Request{AllocationID: allocation.AllocationID("match-1"), FleetID: fleetID}
	const clients = 8
	results := make([]string, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			res, err := a.Allocate(context.Background(), req)
			if err != nil {
				errs[n] = err
				return
			}
			results[n] = res.GameServer.ID
		}(i)
	}
	wg.Wait()

	first := ""
	for n := 0; n < clients; n++ {
		if errs[n] != nil {
			// A racer can transiently see an empty pool or claim contention
			// while the winner's transaction is in flight; both are
			// retryable codes. What must never happen is two different
			// servers for one key.
			if c := connect.CodeOf(errs[n]); c != connect.CodeAborted && c != connect.CodeResourceExhausted {
				t.Fatalf("racer %d: %v", n, errs[n])
			}
			continue
		}
		if first == "" {
			first = results[n]
		}
		if results[n] != first {
			t.Fatalf("same key allocated two servers: %s and %s", first, results[n])
		}
	}
	if first == "" {
		t.Fatal("no racer succeeded")
	}
	// Retry after the dust settles returns the same allocation.
	res, err := a.Allocate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != first || !res.Reused {
		t.Fatalf("post-race resend: got %s (reused=%v), want %s", res.GameServer.ID, res.Reused, first)
	}

	// No inventory was lost to the race: the second server is still (or is
	// back) in the pool and allocatable under a fresh key.
	res2, err := a.Allocate(context.Background(), allocation.Request{
		AllocationID: allocation.AllocationID("match-1-second"), FleetID: fleetID,
	})
	if err != nil {
		t.Fatalf("second server was lost to the idempotency race: %v", err)
	}
	if res2.GameServer.ID == first {
		t.Fatalf("second key re-allocated the same server %s", first)
	}
}

// TestAllocateSkipsConcurrentlyFailedServer: a pooled server that went
// Unhealthy between pop and claim is skipped, never double-assigned.
func TestAllocateSkipsConcurrentlyFailedServer(t *testing.T) {
	st, p := newStore(t), newPool(t)
	fleetID := "f-race-" + t.Name()
	seedReady(t, st, p, "gs-sick", fleetID)
	seedReady(t, st, p, "gs-fine", fleetID)

	// The health reconciler wrote it off, but the stale pool entry remains.
	if _, err := st.TransitionState(context.Background(), "gs-sick", store.StateReady, store.StateUnhealthy, nil); err != nil {
		t.Fatal(err)
	}

	a := allocation.New(st, p, nil)
	res, err := a.Allocate(context.Background(), allocation.Request{
		AllocationID: allocation.AllocationID("match-2"), FleetID: fleetID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-fine" {
		t.Fatalf("allocated %s, want gs-fine", res.GameServer.ID)
	}
}

// TestAllocateDistinctKeysDistinctServers: N clients with distinct keys get
// N distinct servers; the N+1th gets RESOURCE_EXHAUSTED.
func TestAllocateDistinctKeysDistinctServers(t *testing.T) {
	st, p := newStore(t), newPool(t)
	fleetID := "f-many-" + t.Name()
	const n = 5
	for i := 0; i < n; i++ {
		seedReady(t, st, p, fmt.Sprintf("gs-m%d", i), fleetID)
	}
	a := allocation.New(st, p, nil)

	got := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := a.Allocate(context.Background(), allocation.Request{
				AllocationID: allocation.AllocationID(fmt.Sprintf("key-%d", i)), FleetID: fleetID,
			})
			if err != nil {
				t.Errorf("allocate %d: %v", i, err)
				return
			}
			got <- res.GameServer.ID
		}(i)
	}
	wg.Wait()
	close(got)

	seen := map[string]bool{}
	for id := range got {
		if seen[id] {
			t.Fatalf("server %s allocated twice", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("distinct servers = %d, want %d", len(seen), n)
	}

	_, err := a.Allocate(context.Background(), allocation.Request{
		AllocationID: allocation.AllocationID("key-extra"), FleetID: fleetID,
	})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("exhausted err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

// TestReleaseReturnsServerToPool: release transitions Allocated → Ready and
// the server becomes allocatable again through the real pool.
func TestReleaseReturnsServerToPool(t *testing.T) {
	st, p := newStore(t), newPool(t)
	fleetID := "f-rel-" + t.Name()
	seedReady(t, st, p, "gs-rel", fleetID)
	a := allocation.New(st, p, nil)

	allocID := allocation.AllocationID("match-rel")
	if _, err := a.Allocate(context.Background(), allocation.Request{AllocationID: allocID, FleetID: fleetID}); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background(), allocID); err != nil {
		t.Fatal(err)
	}

	res, err := a.Allocate(context.Background(), allocation.Request{
		AllocationID: allocation.AllocationID("match-rel-2"), FleetID: fleetID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-rel" {
		t.Fatalf("re-allocated %s, want gs-rel", res.GameServer.ID)
	}
}
