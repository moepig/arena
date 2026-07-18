package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEmitterWritesEMFDocument(t *testing.T) {
	var buf bytes.Buffer
	e := NewEmitter(&buf)
	e.now = func() time.Time { return time.UnixMilli(1_700_000_000_000) }

	e.Emit("Arena/Fleet", map[string]string{"FleetId": "f1"},
		Datum{Name: "ReadyGameServers", Unit: UnitCount, Value: 7},
	)

	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Fatal("EMF document must be newline-terminated")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["FleetId"] != "f1" {
		t.Errorf("dimension value FleetId = %v", doc["FleetId"])
	}
	if doc["ReadyGameServers"] != 7.0 {
		t.Errorf("metric value = %v, want 7", doc["ReadyGameServers"])
	}
	aws, ok := doc["_aws"].(map[string]any)
	if !ok {
		t.Fatal("missing _aws metadata")
	}
	if aws["Timestamp"] != 1.7e12 {
		t.Errorf("timestamp = %v", aws["Timestamp"])
	}
	cwm := aws["CloudWatchMetrics"].([]any)[0].(map[string]any)
	if cwm["Namespace"] != "Arena/Fleet" {
		t.Errorf("namespace = %v", cwm["Namespace"])
	}
	dims := cwm["Dimensions"].([]any)[0].([]any)
	if len(dims) != 1 || dims[0] != "FleetId" {
		t.Errorf("dimensions = %v, want [FleetId]", dims)
	}
	metrics := cwm["Metrics"].([]any)[0].(map[string]any)
	if metrics["Name"] != "ReadyGameServers" || metrics["Unit"] != "Count" {
		t.Errorf("metric declaration = %v", metrics)
	}
}

func TestNilMetricsAreNoop(t *testing.T) {
	var m *Metrics
	// Must not panic.
	m.FleetGameServers("f1", 1, 2, 3, 4, 5, 6)
	m.ReconcileDuration("f1", time.Second)
	m.UnhealthyGameServer("f1")
	m.Allocation("f1", time.Millisecond, false, false)
}

func TestAllocationOutcomeFlags(t *testing.T) {
	var buf bytes.Buffer
	m := NewMetrics(NewEmitter(&buf))

	m.Allocation("f1", 5*time.Millisecond, true, true)  // pool miss
	m.Allocation("f1", 5*time.Millisecond, false, true) // hard error

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("emitted %d documents, want 2", len(lines))
	}
	var miss, hard map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &miss); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &hard); err != nil {
		t.Fatal(err)
	}
	if miss["PoolMiss"] != 1.0 || miss["AllocationErrors"] != 0.0 {
		t.Errorf("pool miss flags = %v/%v, want 1/0", miss["PoolMiss"], miss["AllocationErrors"])
	}
	if hard["PoolMiss"] != 0.0 || hard["AllocationErrors"] != 1.0 {
		t.Errorf("hard error flags = %v/%v, want 0/1", hard["PoolMiss"], hard["AllocationErrors"])
	}
}
