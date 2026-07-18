package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moepig/arena/internal/store"
)

func TestRebuildPoolsRestoresHealthyReadyServers(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 3)
	addGS(fs, "gs-alive", "f1", store.StateReady, 1_000)
	addGS(fs, "gs-dead", "f1", store.StateReady, 1_000)
	addGS(fs, "gs-alloc", "f1", store.StateAllocated, 1_000)
	fp.heartbeats = map[string]bool{"gs-alive": true, "gs-dead": false}
	c := newTestController(fs, fl, fp)

	if err := c.RebuildPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fp.epoch.Load() != 1 {
		t.Errorf("epoch = %d, want bumped to 1", fp.epoch.Load())
	}
	// Only the Ready server with a live heartbeat is restored; the dead one
	// is left for the health sweep, Allocated servers are never pooled.
	if len(fp.added) != 1 || fp.added[0] != "f1/gs-alive" {
		t.Errorf("restored = %v, want [f1/gs-alive]", fp.added)
	}
	// The fleet is re-enqueued so reconcile mops up.
	if id, ok := c.queue.Get(); !ok || id != "f1" {
		t.Errorf("fleet not re-enqueued (got %q, %v)", id, ok)
	}
}

func TestRebuildLoopTriggersOnRecovery(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateReady, 1_000)
	c := New(fs, fl, fp, nil, Options{
		RedisPingInterval: time.Millisecond,
		RebuildDelay:      time.Millisecond,
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.rebuildLoop(ctx)

	// Healthy → no rebuild.
	time.Sleep(20 * time.Millisecond)
	if fp.epoch.Load() != 0 {
		t.Fatal("rebuild ran while redis stayed healthy")
	}

	// Outage, then recovery → exactly one rebuild.
	fp.setPingErr(errors.New("connection refused"))
	time.Sleep(20 * time.Millisecond)
	fp.setPingErr(nil)
	waitFor(t, "rebuild after recovery", func() bool { return fp.epoch.Load() == 1 })

	// Stays at one rebuild while healthy.
	time.Sleep(20 * time.Millisecond)
	if fp.epoch.Load() != 1 {
		t.Errorf("epoch = %d, want exactly 1 rebuild", fp.epoch.Load())
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
