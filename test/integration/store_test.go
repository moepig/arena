//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moepig/arena/internal/store"
)

// TestConcurrentTransitionSingleWinner: the version-conditioned write lets
// exactly one of N racing transitions succeed.
func TestConcurrentTransitionSingleWinner(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	mustPutGS(t, st, "gs-race", "f1", store.StateStarting)

	const racers = 8
	var wg sync.WaitGroup
	wins := make(chan struct{}, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.TransitionState(ctx, "gs-race", store.StateStarting, store.StateReady, nil); err == nil {
				wins <- struct{}{}
			} else if !errors.Is(err, store.ErrConditionFailed) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	close(wins)
	if got := len(wins); got != 1 {
		t.Fatalf("winners = %d, want exactly 1", got)
	}
	gs, err := st.GetGameServer(ctx, "gs-race")
	if err != nil {
		t.Fatal(err)
	}
	if gs.State != store.StateReady || gs.Version != 3 {
		t.Errorf("final state/version = %s/%d, want Ready/3", gs.State, gs.Version)
	}
}

// TestClaimGameServerTransactional: the Ready → Allocated transition and the
// allocation record commit atomically; N racing claims produce one winner
// and exactly one allocation record.
func TestClaimGameServerTransactional(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	mustPutGS(t, st, "gs-claim", "f1", store.StateReady)

	const racers = 8
	var wg sync.WaitGroup
	winners := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			allocID := string(rune('a'+n)) + "-alloc"
			_, err := st.ClaimGameServer(ctx, "gs-claim", store.Allocation{ID: allocID}, nil)
			if err == nil {
				winners <- allocID
			} else if !errors.Is(err, store.ErrConditionFailed) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	if got := len(winners); got != 1 {
		t.Fatalf("claim winners = %d, want exactly 1", got)
	}
	winner := <-winners

	gs, err := st.GetGameServer(ctx, "gs-claim")
	if err != nil {
		t.Fatal(err)
	}
	if gs.State != store.StateAllocated {
		t.Errorf("state = %s, want Allocated", gs.State)
	}
	alloc, err := st.GetAllocation(ctx, winner)
	if err != nil {
		t.Fatalf("winner allocation record missing: %v", err)
	}
	if alloc.GameServerID != "gs-claim" {
		t.Errorf("allocation points at %q", alloc.GameServerID)
	}
	// Losers must have written nothing (transaction atomicity).
	for n := 0; n < racers; n++ {
		id := string(rune('a'+n)) + "-alloc"
		if id == winner {
			continue
		}
		if _, err := st.GetAllocation(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("loser %s left an allocation record (err=%v)", id, err)
		}
	}
}

// TestAddAllocationTransactional: a high-density reallocation commits an
// additional Allocation record without touching the GameServer, but only
// while it stays Allocated — a concurrent Ready() aborts every racer via
// the ConditionCheck in the same transaction.
func TestAddAllocationTransactional(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	mustPutGS(t, st, "gs-density", "f1", store.StateAllocated)

	const racers = 8
	var wg sync.WaitGroup
	winners := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			allocID := string(rune('a'+n)) + "-additional"
			_, err := st.AddAllocation(ctx, "gs-density", store.Allocation{ID: allocID, Additional: true})
			if err == nil {
				winners <- allocID
			} else if !errors.Is(err, store.ErrConditionFailed) {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(winners)
	if got := len(winners); got != racers {
		t.Fatalf("additional allocation winners = %d, want all %d (GameServer stays Allocated throughout)", got, racers)
	}

	gs, err := st.GetGameServer(ctx, "gs-density")
	if err != nil {
		t.Fatal(err)
	}
	if gs.State != store.StateAllocated {
		t.Errorf("state = %s, want Allocated (unchanged by AddAllocation)", gs.State)
	}
	for w := range winners {
		if _, err := st.GetAllocation(ctx, w); err != nil {
			t.Errorf("winner allocation %s missing: %v", w, err)
		}
	}

	// Once the GameServer leaves Allocated, a new reallocation is rejected.
	if _, err := st.TransitionState(ctx, "gs-density", store.StateAllocated, store.StateReady, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAllocation(ctx, "gs-density", store.Allocation{ID: "late-additional"}); !errors.Is(err, store.ErrConditionFailed) {
		t.Fatalf("err = %v, want ErrConditionFailed (no longer Allocated)", err)
	}
}

// TestFleetIndexStatePrefix: the composite sort key serves per-state listing
// off the fleet-index GSI.
func TestFleetIndexStatePrefix(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	mustPutGS(t, st, "gs-r1", "f-idx", store.StateReady)
	mustPutGS(t, st, "gs-r2", "f-idx", store.StateReady)
	mustPutGS(t, st, "gs-a1", "f-idx", store.StateAllocated)
	mustPutGS(t, st, "gs-s1", "f-idx", store.StateStarting)
	mustPutGS(t, st, "gs-other", "f-other", store.StateReady)

	ready, err := st.ListAllGameServersByFleet(ctx, "f-idx", store.StateReady)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 2 {
		t.Fatalf("ready in f-idx = %d, want 2", len(ready))
	}
	for _, gs := range ready {
		if gs.State != store.StateReady || gs.FleetID != "f-idx" {
			t.Errorf("unexpected result %s/%s", gs.ID, gs.State)
		}
	}
	all, err := st.ListAllGameServersByFleet(ctx, "f-idx", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("all in f-idx = %d, want 4", len(all))
	}
}

// TestFleetVersionConflict: optimistic locking on the fleets table.
func TestFleetVersionConflict(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	f := store.Fleet{ID: "f-ver", Namespace: "default", Name: "f-ver", Replicas: 1, Version: 1}
	if err := st.CreateFleet(ctx, f); err != nil {
		t.Fatal(err)
	}

	cur, err := st.GetFleet(ctx, "f-ver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateFleet(ctx, *cur); err != nil {
		t.Fatal(err)
	}
	// Same (now stale) version again → conflict.
	if _, err := st.UpdateFleet(ctx, *cur); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale update err = %v, want ErrVersionConflict", err)
	}
}

// TestLeaderLease: contention, renewal, explicit release, and expiry
// over the leases table.
func TestLeaderLease(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	const lease = "controller-leader"

	ok, err := st.AcquireLease(ctx, lease, "holder-a", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("initial acquire = %v/%v", ok, err)
	}
	// A live lease refuses another holder, allows renewal by the owner.
	if ok, _ := st.AcquireLease(ctx, lease, "holder-b", 30*time.Second); ok {
		t.Fatal("second holder stole a live lease")
	}
	if ok, _ := st.AcquireLease(ctx, lease, "holder-a", 30*time.Second); !ok {
		t.Fatal("owner could not renew")
	}
	// Explicit release promotes the standby immediately.
	if err := st.ReleaseLease(ctx, lease, "holder-a"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.AcquireLease(ctx, lease, "holder-b", time.Second); !ok {
		t.Fatal("standby could not take a released lease")
	}
	// Expiry: holder-b's 1s lease lapses and holder-a may take over.
	// expires_at has second granularity and the condition is strict (<), so
	// wait past the next full second.
	time.Sleep(2100 * time.Millisecond)
	if ok, _ := st.AcquireLease(ctx, lease, "holder-a", 30*time.Second); !ok {
		t.Fatal("expired lease was not reacquirable")
	}
}
