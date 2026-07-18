package controller

// Tests for the autoscaler policies: Webhook, Chain, Counter.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

func autoscaledFleetJSON(fs *fakeCtrlStore, replicas int32, autoscalingJSON string) *store.Fleet {
	fl := addFleet(fs, "f1", replicas)
	fl.AutoscalingEnabled = true
	fl.AutoscalingJSON = autoscalingJSON
	return fl
}

func TestWebhookAutoscaler(t *testing.T) {
	var gotBody webhookRequest
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Arena-Token") != "s3cret" {
			t.Error("static header not forwarded")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"replicas": 5}`))
	}))
	defer hook.Close()

	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleetJSON(fs, 2, `{"enabled": true, "policy": {"type": "TYPE_WEBHOOK", "webhook": {"url": "`+hook.URL+`", "headers": {"X-Arena-Token": "s3cret"}}}, "minReplicas": 0, "maxReplicas": 10}`)
	addGS(fs, "gs-1", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 5 {
		t.Fatalf("replicas = %d, want webhook's 5", got)
	}
	if gotBody.Status.Allocated != 1 {
		t.Errorf("webhook body status = %+v, want allocated 1", gotBody.Status)
	}
}

func TestWebhookFailureKeepsReplicas(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer hook.Close()

	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleetJSON(fs, 2, `{"enabled": true, "policy": {"type": "TYPE_WEBHOOK", "webhook": {"url": "`+hook.URL+`"}}, "minReplicas": 0, "maxReplicas": 10}`)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 2 {
		t.Fatalf("replicas = %d, want unchanged 2 on webhook failure", got)
	}
}

func TestChainPolicyFallsThroughToDefault(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	// First entry: minute-59 cron with a 60s window — now (Unix 10_000) is
	// never at minute 59 in any real timezone offset, so the window is
	// inactive and the default buffer applies.
	autoscaledFleetJSON(fs, 1, `{"enabled": true, "policy": {"type": "TYPE_CHAIN", "chain": [
	  {"schedule": {"cron": "59 * * * *", "durationSeconds": "60"},
	   "policy": {"type": "TYPE_BUFFER", "buffer": {"bufferSize": 20}}},
	  {"policy": {"type": "TYPE_BUFFER", "buffer": {"bufferSize": 3}}}
	]}, "minReplicas": 0, "maxReplicas": 50}`)
	addGS(fs, "gs-1", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	// default entry: allocated(1) + buffer(3) = 4
	if got := fs.fleets["f1"].Replicas; got != 4 {
		t.Fatalf("replicas = %d, want default chain entry's 4", got)
	}
}

func TestChainPolicyActiveWindowWins(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	// An every-minute cron is always inside its window: the peak entry wins.
	autoscaledFleetJSON(fs, 1, `{"enabled": true, "policy": {"type": "TYPE_CHAIN", "chain": [
	  {"schedule": {"cron": "* * * * *", "durationSeconds": "7200"},
	   "policy": {"type": "TYPE_BUFFER", "buffer": {"bufferSize": 20}}},
	  {"policy": {"type": "TYPE_BUFFER", "buffer": {"bufferSize": 3}}}
	]}, "minReplicas": 0, "maxReplicas": 50}`)
	addGS(fs, "gs-1", "f1", store.StateAllocated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 21 {
		t.Fatalf("replicas = %d, want peak entry's 21", got)
	}
}

func TestCounterDesired(t *testing.T) {
	st := store.FleetStatus{
		Total: 4,
		Counters: map[string]store.CounterAggregate{
			"rooms": {Count: 35, Capacity: 40}, // 5 available, 10 per GS
		},
	}
	p := &arenav1.CounterPolicy{Key: "rooms", BufferSize: 15}
	// buffer 15 - available 5 = 10 short → +1 server (10 per GS).
	if got, ok := counterDesired(p, st, 4); !ok || got != 5 {
		t.Errorf("counterDesired = %d/%v, want 5/true", got, ok)
	}

	// Surplus scales down: available 25, buffer 5 → -2 servers.
	st.Counters["rooms"] = store.CounterAggregate{Count: 15, Capacity: 40}
	p.BufferSize = 5
	if got, ok := counterDesired(p, st, 4); !ok || got != 2 {
		t.Errorf("counterDesired = %d/%v, want 2/true", got, ok)
	}

	// No counter data → no opinion (safe).
	if _, ok := counterDesired(&arenav1.CounterPolicy{Key: "absent", BufferSize: 1}, st, 4); ok {
		t.Error("missing counter data must yield no opinion")
	}
}

// TestCounterAutoscalerEndToEnd drives the full chain:
// Redis Counter snapshots → reconcile's fleet aggregation → the Counter
// autoscaler → fleet.Replicas → the launcher, in one reconcileFleet pass.
func TestCounterAutoscalerEndToEnd(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	autoscaledFleetJSON(fs, 2, `{
		"enabled": true,
		"policy": {"type": "TYPE_COUNTER", "counter": {"key": "rooms", "bufferSize": 15}}
	}`)
	// 2 Ready servers, each reporting rooms 35/40 aggregate → 5 available,
	// 20 per-server capacity (40 total / 2 servers). buffer 15 - available 5
	// = short by 10 → +1 server.
	addGS(fs, "gs-1", "f1", store.StateReady, 1_000)
	addGS(fs, "gs-2", "f1", store.StateReady, 1_000)
	fp.pooled = map[string]bool{"f1/gs-1": true, "f1/gs-2": true}
	fp.counters = map[string]pool.Snapshot{
		"gs-1": {Counters: map[string]pool.Counter{"rooms": {Count: 20, Capacity: 20}}},
		"gs-2": {Counters: map[string]pool.Counter{"rooms": {Count: 15, Capacity: 20}}},
	}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.fleets["f1"].Replicas; got != 3 {
		t.Fatalf("replicas = %d, want 3 (2 current + 1 for the counter shortfall)", got)
	}
	if st := fs.statusIn["f1"]; st.Counters["rooms"] != (store.CounterAggregate{Count: 35, Capacity: 40}) {
		t.Errorf("status.counters[rooms] = %+v, want the aggregated snapshot", st.Counters["rooms"])
	}
}
