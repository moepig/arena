package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestPool(t *testing.T) (*Pool, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	p := New(rdb)
	if err := p.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p, mr
}

func TestPoolFIFO(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	if _, err := p.PopMin(ctx, "f1"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty pool: got %v, want ErrEmpty", err)
	}

	// Insert out of order; pop must be oldest-first.
	if err := p.Add(ctx, "f1", "gs-new", 200, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Add(ctx, "f1", "gs-old", 100, nil); err != nil {
		t.Fatal(err)
	}
	got, err := p.PopMin(ctx, "f1")
	if err != nil || got != "gs-old" {
		t.Fatalf("PopMin = %q, %v; want gs-old", got, err)
	}
	got, err = p.PopMin(ctx, "f1")
	if err != nil || got != "gs-new" {
		t.Fatalf("PopMin = %q, %v; want gs-new", got, err)
	}
	if _, err := p.PopMin(ctx, "f1"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("drained pool: got %v, want ErrEmpty", err)
	}
}

func TestPoolRemoveAndSize(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	_ = p.Add(ctx, "f1", "a", 1, nil)
	_ = p.Add(ctx, "f1", "b", 2, nil)
	if err := p.Remove(ctx, "f1", "a"); err != nil {
		t.Fatal(err)
	}
	n, err := p.Size(ctx, "f1")
	if err != nil || n != 1 {
		t.Fatalf("Size = %d, %v; want 1", n, err)
	}
}

func TestEpochBumpInvalidatesPool(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	_ = p.Add(ctx, "f1", "stale", 1, nil)
	if _, err := p.BumpEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Epoch() != 2 {
		t.Fatalf("Epoch = %d, want 2", p.Epoch())
	}
	// Old-epoch entries are invisible after the bump.
	if _, err := p.PopMin(ctx, "f1"); !errors.Is(err, ErrEmpty) {
		t.Fatalf("post-bump pop: got %v, want ErrEmpty", err)
	}
	// Rebuild inserts land in the new epoch.
	_ = p.Add(ctx, "f1", "fresh", 1, nil)
	got, err := p.PopMin(ctx, "f1")
	if err != nil || got != "fresh" {
		t.Fatalf("PopMin = %q, %v; want fresh", got, err)
	}
}

func TestHeartbeats(t *testing.T) {
	p, mr := newTestPool(t)
	ctx := context.Background()

	if err := p.SetHeartbeat(ctx, "gs-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	alive, err := p.Heartbeats(ctx, []string{"gs-1", "gs-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !alive[0] || alive[1] {
		t.Fatalf("Heartbeats = %v, want [true false]", alive)
	}

	// TTL expiry marks the server dead.
	mr.FastForward(HeartbeatTTL + time.Second)
	alive, err = p.Heartbeats(ctx, []string{"gs-1"})
	if err != nil {
		t.Fatal(err)
	}
	if alive[0] {
		t.Fatal("heartbeat should have expired")
	}
}

func TestAllocationPubSub(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	ch, cancel := p.SubscribeAllocation(ctx, "gs-1")
	defer cancel()
	// Give the subscription a moment to establish before publishing.
	time.Sleep(50 * time.Millisecond)

	if err := p.PublishAllocation(ctx, "gs-1", []byte(`{"id":"a1"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-ch:
		if string(payload) != `{"id":"a1"}` {
			t.Fatalf("payload = %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no allocation push received")
	}
}

func TestSetCountersAndBulkFetch(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	counters := map[string]Counter{"players": {Count: 3, Capacity: 10}}
	lists := map[string]List{"sessions": {Capacity: 5, Values: []string{"a", "b"}}}
	if err := p.SetCounters(ctx, "f1", "gs-1", counters, lists); err != nil {
		t.Fatal(err)
	}
	if err := p.SetCounters(ctx, "f1", "gs-2", map[string]Counter{"players": {Count: 7, Capacity: 10}}, nil); err != nil {
		t.Fatal(err)
	}

	snaps, err := p.Counters(ctx, []string{"gs-1", "gs-2", "gs-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2 (missing id must be omitted): %+v", len(snaps), snaps)
	}
	got := snaps["gs-1"]
	if got.Counters["players"] != (Counter{Count: 3, Capacity: 10}) {
		t.Fatalf("gs-1 players = %+v", got.Counters["players"])
	}
	if l := got.Lists["sessions"]; l.Capacity != 5 || len(l.Values) != 2 {
		t.Fatalf("gs-1 sessions list = %+v", l)
	}
	if got2 := snaps["gs-2"].Counters["players"]; got2.Count != 7 {
		t.Fatalf("gs-2 players = %+v", got2)
	}
	if _, ok := snaps["gs-missing"]; ok {
		t.Fatal("gs-missing should be absent")
	}
}

func TestReserveAndReleaseCounter(t *testing.T) {
	p, _ := newTestPool(t)
	ctx := context.Background()

	if err := p.SetCounters(ctx, "f1", "gs-1", map[string]Counter{"rooms": {Count: 0, Capacity: 2}}, nil); err != nil {
		t.Fatal(err)
	}

	ok, err := p.ReserveCounter(ctx, "f1", "rooms", "gs-1", 1)
	if err != nil || !ok {
		t.Fatalf("first reserve = %v, %v; want ok", ok, err)
	}
	ok, err = p.ReserveCounter(ctx, "f1", "rooms", "gs-1", 1)
	if err != nil || !ok {
		t.Fatalf("second reserve = %v, %v; want ok (2 available)", ok, err)
	}
	ok, err = p.ReserveCounter(ctx, "f1", "rooms", "gs-1", 1)
	if err != nil || ok {
		t.Fatalf("third reserve = %v, %v; want false (capacity exhausted)", ok, err)
	}

	if err := p.ReleaseCounter(ctx, "f1", "rooms", "gs-1", 1); err != nil {
		t.Fatal(err)
	}
	ok, err = p.ReserveCounter(ctx, "f1", "rooms", "gs-1", 1)
	if err != nil || !ok {
		t.Fatalf("reserve after release = %v, %v; want ok", ok, err)
	}

	ok, err = p.ReserveCounter(ctx, "f1", "rooms", "gs-missing", 1)
	if err != nil || ok {
		t.Fatalf("reserve on unseeded gs = %v, %v; want false, nil (fail closed)", ok, err)
	}
}
