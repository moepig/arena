package store

// State is a GameServer lifecycle state.
type State string

const (
	StateScheduled  State = "Scheduled"
	StateStarting   State = "Starting"
	StateReady      State = "Ready"
	StateAllocated  State = "Allocated"
	StateReserved   State = "Reserved"
	StateDraining   State = "Draining"
	StateUnhealthy  State = "Unhealthy"
	StateTerminated State = "Terminated"
)

// allowedTransitions encodes the GameServer state machine's legal
// transitions. Every state may reach Terminated directly (task stop
// confirmation).
var allowedTransitions = map[State]map[State]bool{
	StateScheduled: {StateStarting: true, StateUnhealthy: true, StateTerminated: true},
	StateStarting:  {StateReady: true, StateUnhealthy: true, StateTerminated: true},
	StateReady:     {StateAllocated: true, StateReserved: true, StateDraining: true, StateUnhealthy: true, StateTerminated: true},
	StateAllocated: {StateReady: true, StateDraining: true, StateUnhealthy: true, StateTerminated: true},
	// Reserved → Reserved extends/renews the reservation window.
	StateReserved:  {StateReserved: true, StateReady: true, StateAllocated: true, StateDraining: true, StateUnhealthy: true, StateTerminated: true},
	StateDraining:  {StateUnhealthy: true, StateTerminated: true},
	StateUnhealthy: {StateTerminated: true},
}

// CanTransition reports whether from → to is a legal transition. The store
// additionally enforces this server-side via ConditionExpression; violations
// are rejected and left to reconcile.
func CanTransition(from, to State) bool {
	return allowedTransitions[from][to]
}
