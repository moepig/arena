package controller

// Fleet reconciliation sharding tests. These drive leadShard directly
// (rather than the full Run/lease-acquisition loop) so they stay
// deterministic and fast; the integration suite covers two real Controller
// processes actually splitting shards via DynamoDB leases.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// distinctShardFleetIDs returns two fleet ids that FleetShard places on
// different shards (deterministic hash, so this pair is stable across
// runs).
func distinctShardFleetIDs(t *testing.T, shardCount int) (a, b string) {
	t.Helper()
	a = "fleet-0"
	for i := 1; i < 1000; i++ {
		id := fmt.Sprintf("fleet-%d", i)
		if FleetShard(id, shardCount) != FleetShard(a, shardCount) {
			return a, id
		}
	}
	t.Fatalf("could not find two fleet ids on different shards out of %d", shardCount)
	return "", ""
}

func TestLeadShardOnlyReconcilesOwnedFleets(t *testing.T) {
	const shardCount = 2
	idA, idB := distinctShardFleetIDs(t, shardCount)
	shardA := FleetShard(idA, shardCount)

	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, idA, 1)
	addFleet(fs, idB, 1)
	c := New(fs, fl, fp, nil, Options{ShardCount: shardCount, Workers: 1, ResyncInterval: time.Hour}, nil)
	c.shardQueues = make([]*workQueue, shardCount) // normally Run's job before spawning shard goroutines

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.leadShard(ctx, shardA, "test-shard-lease")
		close(done)
	}()

	waitFor(t, "shard's fleet gets a launched task", func() bool {
		return fl.launchedCount() >= 1
	})
	if n := fl.launchedCount(); n != 1 {
		t.Fatalf("launched = %d, want exactly 1 (only fleet %s, on shard %d)", n, idA, shardA)
	}

	cancel()
	<-done // synchronizes with the worker goroutine before touching fs directly below

	if got := fs.fleets[idB].Status; got.Total != 0 {
		t.Errorf("fleet %s status = %+v, want untouched (belongs to a different shard)", idB, got)
	}
	c.shardMu.RLock()
	q := c.shardQueues[shardA]
	c.shardMu.RUnlock()
	if q != nil {
		t.Error("shardQueues entry not cleared after leadShard returned")
	}
}

func TestEnqueueRoutesToOwningShardWhenHeld(t *testing.T) {
	const shardCount = 2
	idA, idB := distinctShardFleetIDs(t, shardCount)
	shardA := FleetShard(idA, shardCount)
	shardB := FleetShard(idB, shardCount)

	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	c := New(fs, fl, fp, nil, Options{ShardCount: shardCount}, nil)
	c.shardQueues = make([]*workQueue, shardCount)
	qA := newWorkQueue()
	c.shardQueues[shardA] = qA
	// shardB left nil: this process doesn't currently hold it.

	c.Enqueue(idA)
	c.Enqueue(idB)
	qA.ShutDown() // Get() blocks on empty otherwise; safe after the sync Enqueue calls above

	if got, ok := qA.Get(); !ok || got != idA {
		t.Fatalf("shard %d queue = %q/%v, want %s enqueued", shardA, got, ok, idA)
	}
	if _, ok := qA.Get(); ok {
		t.Errorf("shard %d queue got a second item; %s (shard %d, unheld) must have been dropped", shardA, idB, shardB)
	}
}

func TestResyncOnlyEnqueuesShardsFleets(t *testing.T) {
	const shardCount = 3
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	ids := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("fleet-%d", i)
		addFleet(fs, id, 1)
		ids = append(ids, id)
	}
	c := New(fs, fl, fp, nil, Options{ShardCount: shardCount}, nil)

	for shard := 0; shard < shardCount; shard++ {
		q := newWorkQueue()
		c.resync(context.Background(), shard, q)
		q.ShutDown() // Get() blocks on empty otherwise; safe after the sync resync call above

		want := 0
		for _, id := range ids {
			if FleetShard(id, shardCount) == shard {
				want++
			}
		}
		got := 0
		for {
			if _, ok := q.Get(); !ok {
				break
			}
			got++
		}
		if got != want {
			t.Errorf("shard %d: resync enqueued %d fleets, want %d (FleetShard partition)", shard, got, want)
		}
	}
}
