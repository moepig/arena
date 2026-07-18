package allocation

// Selector semantics: fallback chain, required/preferred
// requirements, match_fields, and the game_server_metadata patch.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/moepig/arena/internal/store"
)

func TestSelectorFallbackChain(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-a"] = readyGS("gs-a", "f1", map[string]string{"version": "v1"})
	a := New(fs, fp, nil)

	// First selector matches nothing; the chain falls back to the second.
	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{
			{MatchLabels: map[string]string{"version": "v9"}},
			{MatchLabels: map[string]string{"version": "v1"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-a" {
		t.Fatalf("allocated %s, want gs-a via fallback selector", res.GameServer.ID)
	}
}

func TestSelectorRequiredAndPreferred(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	// Both match required (region exists); only gs-b matches preferred.
	gsA := readyGS("gs-a", "f1", map[string]string{"region": "ap", "tier": "bronze"})
	gsA.ReadyAt = 10 // older — would win on age alone
	gsB := readyGS("gs-b", "f1", map[string]string{"region": "ap", "tier": "gold"})
	gsB.ReadyAt = 20
	fs.gameservers["gs-a"], fs.gameservers["gs-b"] = gsA, gsB
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{
			Required:  []Requirement{{Key: "region", Operator: OpExists}},
			Preferred: []Requirement{{Key: "tier", Operator: OpEquals, Values: []string{"gold"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-b" {
		t.Fatalf("allocated %s, want gs-b (preferred match ranks first)", res.GameServer.ID)
	}

	// A required NotIn that excludes both → RESOURCE_EXHAUSTED.
	_, err = a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k2"), FleetID: "f1",
		Selectors: []Selector{{
			Required: []Requirement{{Key: "tier", Operator: OpNotIn, Values: []string{"gold", "bronze"}}},
		}},
	})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("err = %v, want RESOURCE_EXHAUSTED", err)
	}
}

func TestSelectorMatchFields(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	gsA := readyGS("gs-a", "f1", nil)
	gsA.SpecHash = "h-old"
	gsB := readyGS("gs-b", "f1", nil)
	gsB.SpecHash = "h-new"
	fs.gameservers["gs-a"], fs.gameservers["gs-b"] = gsA, gsB
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{MatchFields: map[string]string{"spec_hash": "h-new"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-b" {
		t.Fatalf("allocated %s, want gs-b (spec_hash field selector)", res.GameServer.ID)
	}

	res, err = a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k2"), FleetID: "f1",
		Selectors: []Selector{{MatchFields: map[string]string{"id": "gs-a"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-a" {
		t.Fatalf("allocated %s, want gs-a (id field selector)", res.GameServer.ID)
	}
}

func TestAllocatePatchesGameServerMetadata(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", map[string]string{"existing": "label"})
	fp.queues["f1"] = []string{"gs-1"}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		PatchLabels:      map[string]string{"map": "de_dust2"},
		PatchAnnotations: map[string]string{"session": "s-42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gs := fs.gameservers["gs-1"]
	if gs.Labels["map"] != "de_dust2" || gs.Labels["existing"] != "label" {
		t.Errorf("labels = %v, want patch merged with existing", gs.Labels)
	}
	if gs.Annotations["session"] != "s-42" {
		t.Errorf("annotations = %v, want session patch", gs.Annotations)
	}
	if res.GameServer.Labels["map"] != "de_dust2" {
		t.Errorf("result labels = %v, want patch visible in the response", res.GameServer.Labels)
	}
}

func TestLabelSelectorServedFromSubpool(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	labels := map[string]string{"version": "v2", "mode": "ranked"}
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", labels)
	// Pool the server the way the Ready path does: main pool + sub-pool.
	if err := fp.Add(context.Background(), "f1", "gs-1", 100, labels); err != nil {
		t.Fatal(err)
	}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{MatchLabels: labels}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-1" {
		t.Fatalf("allocated %s, want gs-1", res.GameServer.ID)
	}
	if fs.listCalls != 0 {
		t.Errorf("GSI queries = %d, want 0 (sub-pool hit)", fs.listCalls)
	}
	if len(fp.queues["f1"]) != 0 {
		t.Errorf("main pool = %v, want winner removed", fp.queues["f1"])
	}
}

func TestSubpoolStaleEntryFallsBackToGSI(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	labels := map[string]string{"version": "v2"}
	// The sub-pool has a stale entry (server no longer Ready); the matching
	// Ready server is only reachable via the GSI scan.
	stale := readyGS("gs-stale", "f1", labels)
	stale.State = store.StateAllocated
	fs.gameservers["gs-stale"] = stale
	fs.gameservers["gs-live"] = readyGS("gs-live", "f1", labels)
	if err := fp.Add(context.Background(), "f1", "gs-stale", 100, labels); err != nil {
		t.Fatal(err)
	}
	a := New(fs, fp, nil)

	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{MatchLabels: labels}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-live" {
		t.Fatalf("allocated %s, want gs-live via GSI fallback", res.GameServer.ID)
	}
	if fs.listCalls != 1 {
		t.Errorf("GSI queries = %d, want 1 (sub-pool exhausted)", fs.listCalls)
	}
}

func TestEmptySelectorsUseFastPath(t *testing.T) {
	fs, fp := newFakeStore(), newFakePool()
	fs.gameservers["gs-1"] = readyGS("gs-1", "f1", nil)
	fp.queues["f1"] = []string{"gs-1"}
	a := New(fs, fp, nil)

	// A single match-everything selector must not force the GSI slow path.
	res, err := a.Allocate(context.Background(), Request{
		AllocationID: AllocationID("k1"), FleetID: "f1",
		Selectors: []Selector{{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != "gs-1" {
		t.Fatalf("allocated %s, want gs-1", res.GameServer.ID)
	}
	if len(fp.queues["f1"]) != 0 {
		t.Errorf("pool = %v, want candidate popped by the fast path", fp.queues["f1"])
	}
}
