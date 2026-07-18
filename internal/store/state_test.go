package store

import "testing"

func TestCanTransition(t *testing.T) {
	all := []State{
		StateScheduled, StateStarting, StateReady, StateAllocated,
		StateReserved, StateDraining, StateUnhealthy, StateTerminated,
	}

	allowed := map[[2]State]bool{
		{StateScheduled, StateStarting}:   true,
		{StateScheduled, StateUnhealthy}:  true,
		{StateScheduled, StateTerminated}: true,
		{StateStarting, StateReady}:       true,
		{StateStarting, StateUnhealthy}:   true,
		{StateStarting, StateTerminated}:  true,
		{StateReady, StateAllocated}:      true,
		{StateReady, StateReserved}:       true,
		{StateReady, StateDraining}:       true,
		{StateReady, StateUnhealthy}:      true,
		{StateReady, StateTerminated}:     true,
		{StateAllocated, StateReady}:      true,
		{StateAllocated, StateDraining}:   true,
		{StateAllocated, StateUnhealthy}:  true,
		{StateAllocated, StateTerminated}: true,
		// Reserved → Reserved renews the reservation.
		{StateReserved, StateReserved}:    true,
		{StateReserved, StateReady}:       true,
		{StateReserved, StateAllocated}:   true,
		{StateReserved, StateDraining}:    true,
		{StateReserved, StateUnhealthy}:   true,
		{StateReserved, StateTerminated}:  true,
		{StateDraining, StateUnhealthy}:   true,
		{StateDraining, StateTerminated}:  true,
		{StateUnhealthy, StateTerminated}: true,
	}

	for _, from := range all {
		for _, to := range all {
			want := allowed[[2]State{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, want)
			}
		}
	}
}
