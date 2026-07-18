package controller

// Tests for reconciler behavior: Reserved sweep and scale-down protection,
// rolling updates (surge/unavailable, Recreate), allocationOverflow
// metadata, and health.disabled.

import (
	"context"
	"testing"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

func TestReservedExpirySweepReturnsToReady(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	gs := addGS(fs, "gs-1", "f1", store.StateReserved, 100)
	gs.ReadyAt = 100
	gs.ReservedUntil = 9_000 // now is 10_000
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateReady {
		t.Fatalf("state = %s, want Ready (reservation expired)", got)
	}
	if len(fp.added) != 1 {
		t.Errorf("pool adds = %v, want the expired reservation pooled", fp.added)
	}
	if st := fs.statusIn["f1"]; st.Ready != 1 || st.Reserved != 0 {
		t.Errorf("status = %+v, want Ready:1 Reserved:0", st)
	}
}

func TestReservedIndefiniteAndUnexpiredKept(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	addGS(fs, "gs-1", "f1", store.StateReserved, 100).ReservedUntil = 0      // indefinite
	addGS(fs, "gs-2", "f1", store.StateReserved, 100).ReservedUntil = 20_000 // future
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if st := fs.statusIn["f1"]; st.Reserved != 2 || st.Total != 2 {
		t.Errorf("status = %+v, want both still Reserved", st)
	}
	if len(fl.launched) != 0 {
		t.Errorf("launched = %v, want none (reserved count toward replicas)", fl.launched)
	}
}

func TestReservedProtectedFromScaleDown(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 0)
	addGS(fs, "gs-r", "f1", store.StateReserved, 100)
	addGS(fs, "gs-ready", "f1", store.StateReady, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-r"].State; got != store.StateReserved {
		t.Fatalf("reserved server state = %s, want untouched by scale-down", got)
	}
	if got := fs.gameservers["gs-ready"].State; got != store.StateDraining {
		t.Fatalf("ready server state = %s, want Draining (the only drain candidate)", got)
	}
}

// setupRollout: fleet on a new spec_hash with old-generation servers.
func setupRollout(fs *fakeCtrlStore, replicas int32, strategyJSON string) *store.Fleet {
	fl := addFleet(fs, "f1", replicas)
	fl.SpecHash = "h-new"
	fl.StrategyJSON = strategyJSON
	return fl
}

func TestRollingUpdateSurgesAndDrains(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	setupRollout(fs, 4, "")
	for _, id := range []string{"gs-1", "gs-2", "gs-3", "gs-4"} {
		addGS(fs, id, "f1", store.StateReady, 100) // spec_hash "" = old gen
	}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	// Defaults 25%/25% on 4 replicas: surge 1, unavailable 1.
	if len(fl.launched) != 1 {
		t.Fatalf("launched = %v, want exactly 1 (surge budget)", fl.launched)
	}
	drained := 0
	for _, gs := range fs.gameservers {
		if gs.State == store.StateDraining {
			drained++
		}
	}
	if drained != 1 {
		t.Fatalf("drained = %d, want exactly 1 (unavailable budget)", drained)
	}
	if st := fs.statusIn["f1"]; st.Updated != 0 {
		t.Errorf("status.Updated = %d, want 0 before new gen turns active... got status %+v", st.Updated, st)
	}
}

func TestRollingUpdateCompletesWhenOldGone(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet := setupRollout(fs, 2, "")
	for _, id := range []string{"gs-n1", "gs-n2"} {
		addGS(fs, id, "f1", store.StateReady, 100).SpecHash = fleet.SpecHash
	}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fl.launched) != 0 || len(fl.stopped) != 0 {
		t.Errorf("launched %v stopped %v, want steady state", fl.launched, fl.stopped)
	}
	if st := fs.statusIn["f1"]; st.Updated != 2 || st.Total != 2 {
		t.Errorf("status = %+v, want Updated:2 Total:2", st)
	}
}

func TestRecreateDrainsAllOldReadyAtOnce(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	setupRollout(fs, 3, `{"type":"TYPE_RECREATE"}`)
	for _, id := range []string{"gs-1", "gs-2", "gs-3"} {
		addGS(fs, id, "f1", store.StateReady, 100)
	}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gs-1", "gs-2", "gs-3"} {
		if got := fs.gameservers[id].State; got != store.StateDraining {
			t.Errorf("%s state = %s, want Draining (Recreate)", id, got)
		}
	}
	if len(fl.launched) != 3 {
		t.Errorf("launched = %v, want 3 new-generation servers", fl.launched)
	}
}

func TestRollingUpdateNeverDrainsAllocated(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	setupRollout(fs, 1, "")
	addGS(fs, "gs-old", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-old"].State; got != store.StateAllocated {
		t.Fatalf("old allocated state = %s, want untouched during rollout", got)
	}
	// Surge lets the replacement start while the session finishes.
	if len(fl.launched) != 1 {
		t.Errorf("launched = %v, want 1 (surge)", fl.launched)
	}
}

func TestRolloutDrainTimeoutForcesOldAllocated(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet := setupRollout(fs, 1, `{"rollingUpdate":{"drainTimeoutSeconds":"60"}}`)
	fleet.GenerationAt = 100 // now (10_000) is far past 100+60
	addGS(fs, "gs-old", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-old"].State; got != store.StateDraining {
		t.Fatalf("state = %s, want Draining (drain timeout exceeded)", got)
	}

	// Control: within the deadline the session is left alone.
	fs2, fl2, fp2 := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet2 := setupRollout(fs2, 1, `{"rollingUpdate":{"drainTimeoutSeconds":"60"}}`)
	fleet2.GenerationAt = 9_990 // deadline 10_050 > now 10_000
	addGS(fs2, "gs-old", "f1", store.StateAllocated, 100)
	c2 := newTestController(fs2, fl2, fp2)
	if err := c2.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs2.gameservers["gs-old"].State; got != store.StateAllocated {
		t.Fatalf("state = %s, want Allocated (within drain timeout)", got)
	}
}

func TestAllocationOverflowMetadata(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet := setupRollout(fs, 1, "")
	fleet.OverflowJSON = `{"labels":{"arena.dev/overflow":"true"},"annotations":{"note":"drain soon"}}`
	addGS(fs, "gs-old", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	gs := fs.gameservers["gs-old"]
	if gs.Labels["arena.dev/overflow"] != "true" || gs.Annotations["note"] != "drain soon" {
		t.Fatalf("metadata = %v / %v, want overflow labels+annotations applied", gs.Labels, gs.Annotations)
	}
	if len(fp.published) != 1 || fp.published[0] != "gs-old" {
		t.Fatalf("published = %v, want one watch push for gs-old", fp.published)
	}

	// Second pass is idempotent: no second push.
	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fp.published) != 1 {
		t.Errorf("published = %v, want no repeat push once applied", fp.published)
	}
}

func TestOverflowOnScaleDownExcessAllocated(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet := addFleet(fs, "f1", 1)
	fleet.OverflowJSON = `{"labels":{"arena.dev/overflow":"true"}}`
	oldGS := addGS(fs, "gs-a1", "f1", store.StateAllocated, 100)
	oldGS.AllocatedAt = 100
	newGS := addGS(fs, "gs-a2", "f1", store.StateAllocated, 200)
	newGS.AllocatedAt = 200
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	// 2 allocated > 1 replica: the oldest allocation gets the overflow mark.
	if fs.gameservers["gs-a1"].Labels["arena.dev/overflow"] != "true" {
		t.Error("oldest excess allocated server must carry the overflow label")
	}
	if fs.gameservers["gs-a2"].Labels["arena.dev/overflow"] == "true" {
		t.Error("server within replicas must not carry the overflow label")
	}
}

type fakeInstances struct{ gs []string }

func (f fakeInstances) GameServersOnInstance(context.Context, string) ([]string, error) {
	return f.gs, nil
}

func TestSpotInterruptionDrainsInstanceServers(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	addGS(fs, "gs-a", "f1", store.StateReady, 100)
	addGS(fs, "gs-b", "f1", store.StateAllocated, 100)
	fp.pooled = map[string]bool{"f1/gs-a": true}
	c := newTestController(fs, fl, fp)
	c.instances = fakeInstances{gs: []string{"gs-a", "gs-b", "gs-gone"}}

	if err := c.handleSpotInterruption(context.Background(), "i-0abc"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gs-a", "gs-b"} {
		if got := fs.gameservers[id].State; got != store.StateDraining {
			t.Errorf("%s state = %s, want Draining", id, got)
		}
	}
	if fp.pooled["f1/gs-a"] {
		t.Error("drained ready server must leave the pool")
	}
	if len(fp.published) != 2 {
		t.Errorf("published = %v, want Draining pushed to both watch streams", fp.published)
	}
}

func TestHealthDisabledSkipsHeartbeatSweep(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	fleet := addFleet(fs, "f1", 1)
	fleet.TemplateJSON = `{"spec":{"container":{"image":"img"},"health":{"disabled":true}}}`
	addGS(fs, "gs-1", "f1", store.StateReady, 100) // grace long past at now=10_000
	fp.heartbeats = map[string]bool{"gs-1": false} // no heartbeat in Redis
	fp.pooled = map[string]bool{"f1/gs-1": true}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateReady {
		t.Fatalf("state = %s, want Ready (health sweep disabled)", got)
	}

	// Control: with health enabled the same server is failed.
	fleet.TemplateJSON = `{"spec":{"container":{"image":"img"}}}`
	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateUnhealthy {
		t.Fatalf("state = %s, want Unhealthy once health checks are on", got)
	}
}

// TestReconcileAggregatesCounters covers the fleet-wide Counter rollup:
// per-server Redis snapshots for every live (Ready /
// Allocated / Reserved) server are summed into FleetStatus.counters, and a
// server with no reported snapshot contributes nothing (not a hard error).
func TestReconcileAggregatesCounters(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	addGS(fs, "gs-1", "f1", store.StateReady, 100)
	addGS(fs, "gs-2", "f1", store.StateAllocated, 100)
	fp.pooled = map[string]bool{"f1/gs-1": true}
	fp.counters = map[string]pool.Snapshot{
		"gs-1": {Counters: map[string]pool.Counter{"players": {Count: 2, Capacity: 4}}},
		"gs-2": {Counters: map[string]pool.Counter{"players": {Count: 3, Capacity: 4}}},
		// gs-3 has never reported: absent from the fake, must not appear.
	}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	st := fs.statusIn["f1"]
	agg, ok := st.Counters["players"]
	if !ok {
		t.Fatalf("status.counters = %+v, want a players entry", st.Counters)
	}
	if agg.Count != 5 || agg.Capacity != 8 {
		t.Fatalf("players aggregate = %+v, want Count:5 Capacity:8", agg)
	}
}
