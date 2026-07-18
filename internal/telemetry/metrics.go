package telemetry

import "time"

// Metrics is the typed facade over the arena metric set. A nil *Metrics
// is a no-op, so components take it as an optional dependency.
type Metrics struct {
	e *Emitter
}

// NewMetrics returns a Metrics facade over an emitter.
func NewMetrics(e *Emitter) *Metrics { return &Metrics{e: e} }

func fleetDims(fleetID string) map[string]string {
	return map[string]string{"FleetId": fleetID}
}

// FleetGameServers reports the observed per-state counts (Arena/Fleet),
// including Reserved and the current-generation (rolling update) count.
func (m *Metrics) FleetGameServers(fleetID string, total, ready, allocated, starting, reserved, updated int32) {
	if m == nil {
		return
	}
	m.e.Emit("Arena/Fleet", fleetDims(fleetID),
		Datum{Name: "TotalGameServers", Unit: UnitCount, Value: float64(total)},
		Datum{Name: "ReadyGameServers", Unit: UnitCount, Value: float64(ready)},
		Datum{Name: "AllocatedGameServers", Unit: UnitCount, Value: float64(allocated)},
		Datum{Name: "StartingGameServers", Unit: UnitCount, Value: float64(starting)},
		Datum{Name: "ReservedGameServers", Unit: UnitCount, Value: float64(reserved)},
		Datum{Name: "UpdatedGameServers", Unit: UnitCount, Value: float64(updated)},
	)
}

// ReconcileDuration reports one fleet reconcile pass (Arena/Controller).
func (m *Metrics) ReconcileDuration(fleetID string, d time.Duration) {
	if m == nil {
		return
	}
	m.e.Emit("Arena/Controller", fleetDims(fleetID),
		Datum{Name: "ReconcileDuration", Unit: UnitMilliseconds, Value: float64(d.Milliseconds())},
	)
}

// HeartbeatTimeout counts a heartbeat-expiry detection (Arena/Health).
func (m *Metrics) HeartbeatTimeout(fleetID string) {
	if m == nil {
		return
	}
	m.e.Emit("Arena/Health", fleetDims(fleetID),
		Datum{Name: "HeartbeatTimeouts", Unit: UnitCount, Value: 1},
	)
}

// UnhealthyGameServer counts a server written off (Arena/Health).
func (m *Metrics) UnhealthyGameServer(fleetID string) {
	if m == nil {
		return
	}
	m.e.Emit("Arena/Health", fleetDims(fleetID),
		Datum{Name: "UnhealthyGameServers", Unit: UnitCount, Value: 1},
	)
}

// AutoscaleDesired reports the autoscaler's computed target and any scale
// event it implies (Arena/Autoscaler).
func (m *Metrics) AutoscaleDesired(fleetID string, desired, current int32) {
	if m == nil {
		return
	}
	m.e.Emit("Arena/Autoscaler", fleetDims(fleetID),
		Datum{Name: "DesiredReplicas", Unit: UnitCount, Value: float64(desired)},
		Datum{Name: "ScaleUpEvents", Unit: UnitCount, Value: b2f(desired > current)},
		Datum{Name: "ScaleDownEvents", Unit: UnitCount, Value: b2f(desired < current)},
	)
}

// Allocation reports one allocation attempt (Arena/Allocation): latency
// always, plus PoolMiss (no Ready inventory) or AllocationErrors when the
// attempt failed.
func (m *Metrics) Allocation(fleetID string, d time.Duration, poolMiss bool, failed bool) {
	if m == nil {
		return
	}
	data := []Datum{
		{Name: "AllocationLatency", Unit: UnitMilliseconds, Value: float64(d.Milliseconds())},
		{Name: "PoolMiss", Unit: UnitCount, Value: b2f(poolMiss)},
		{Name: "AllocationErrors", Unit: UnitCount, Value: b2f(failed && !poolMiss)},
	}
	m.e.Emit("Arena/Allocation", fleetDims(fleetID), data...)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
