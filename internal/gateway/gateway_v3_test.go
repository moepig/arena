package gateway

// Flows: Reserve, self-Allocate, and returning an Allocated
// server to the pool with Ready().

import (
	"context"
	"testing"
	"time"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// openSession sends the hello for gsID and consumes the initial state push.
func openSession(t *testing.T, fs *fakeStore, pl *pool.Pool, gsID string) (*sessionStream, context.CancelFunc) {
	t.Helper()
	client := startGateway(t, fs, pl)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stream := client.Session(ctx)
	if err := stream.Send(&gatewayv1.SidecarMessage{GameserverId: gsID}); err != nil {
		t.Fatal(err)
	}
	receiveState(t, stream)
	return &sessionStream{t: t, gsID: gsID, stream: stream}, cancel
}

type sessionStream struct {
	t      *testing.T
	gsID   string
	stream interface {
		Send(*gatewayv1.SidecarMessage) error
		Receive() (*gatewayv1.GatewayMessage, error)
	}
}

func (s *sessionStream) send(msg *gatewayv1.SidecarMessage) {
	s.t.Helper()
	msg.GameserverId = s.gsID
	if err := s.stream.Send(msg); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sessionStream) state() *arenav1.GameServer {
	s.t.Helper()
	for i := 0; i < 10; i++ {
		msg, err := s.stream.Receive()
		if err != nil {
			s.t.Fatalf("receive: %v", err)
		}
		if st := msg.GetState(); st != nil {
			return st
		}
	}
	s.t.Fatal("no state push within 10 messages")
	return nil
}

func TestSessionReserveFlow(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateReady, ReadyAt: 100, Version: 1,
	}
	ctx := context.Background()
	if err := pl.Add(ctx, "f1", "gs-1", 100, nil); err != nil {
		t.Fatal(err)
	}
	ss, cancel := openSession(t, fs, pl, "gs-1")
	defer cancel()

	// Reserve(60s): Reserved with a deadline, out of the pool.
	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Reserve{Reserve: &gatewayv1.ReserveRequest{Seconds: 60}},
	})
	st := ss.state()
	if st.GetState() != arenav1.GameServer_STATE_RESERVED {
		t.Fatalf("state = %v, want RESERVED", st.GetState())
	}
	if st.GetReservedUntil() <= time.Now().Unix() {
		t.Errorf("reserved_until = %d, want a future deadline", st.GetReservedUntil())
	}
	if pooled, _ := pl.Contains(ctx, "f1", "gs-1"); pooled {
		t.Error("reserved server must leave the ready pool")
	}

	// Extending while Reserved is allowed (Reserved → Reserved).
	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Reserve{Reserve: &gatewayv1.ReserveRequest{Seconds: 0}},
	})
	if st := ss.state(); st.GetState() != arenav1.GameServer_STATE_RESERVED || st.GetReservedUntil() != 0 {
		t.Fatalf("state = %v until %d, want RESERVED with indefinite reservation", st.GetState(), st.GetReservedUntil())
	}

	// Ready() ends the reservation and pools the server again.
	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}},
	})
	if st := ss.state(); st.GetState() != arenav1.GameServer_STATE_READY {
		t.Fatalf("state = %v, want READY", st.GetState())
	}
	if pooled, _ := pl.Contains(ctx, "f1", "gs-1"); !pooled {
		t.Error("server must re-enter the pool after ending the reservation")
	}
}

func TestSessionSelfAllocate(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateReady, ReadyAt: 100, Version: 1,
	}
	ctx := context.Background()
	if err := pl.Add(ctx, "f1", "gs-1", 100, nil); err != nil {
		t.Fatal(err)
	}
	ss, cancel := openSession(t, fs, pl, "gs-1")
	defer cancel()

	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Allocate{Allocate: &gatewayv1.SelfAllocateRequest{}},
	})
	if st := ss.state(); st.GetState() != arenav1.GameServer_STATE_ALLOCATED {
		t.Fatalf("state = %v, want ALLOCATED", st.GetState())
	}
	if pooled, _ := pl.Contains(ctx, "f1", "gs-1"); pooled {
		t.Error("self-allocated server must leave the pool")
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.allocs) != 1 {
		t.Fatalf("allocations = %d, want 1 synthesized record", len(fs.allocs))
	}
	if fs.allocs[0].Metadata["arena.dev/self-allocated"] != "true" {
		t.Errorf("allocation metadata = %v, want arena.dev/self-allocated=true", fs.allocs[0].Metadata)
	}
}

func TestSessionReadyFromAllocatedReleasesAndPools(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateAllocated, ReadyAt: 100, AllocatedAt: 200, Version: 1,
	}
	ss, cancel := openSession(t, fs, pl, "gs-1")
	defer cancel()

	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}},
	})
	if st := ss.state(); st.GetState() != arenav1.GameServer_STATE_READY {
		t.Fatalf("state = %v, want READY (reuse)", st.GetState())
	}
	if pooled, _ := pl.Contains(context.Background(), "f1", "gs-1"); !pooled {
		t.Error("reused server must re-enter the pool")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.released) != 1 || fs.released[0] != "gs-1" {
		t.Errorf("released = %v, want active allocations of gs-1 released", fs.released)
	}
}

// TestSessionCountersSyncPersists exercises the Counter/List full-state sync
// path: the sidecar's CountersSync message has no
// gameserver_id-adjacent fleet_id, so the gateway must resolve it from
// DynamoDB and persist the snapshot under the right fleet-scoped keys.
func TestSessionCountersSyncPersists(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateReady, ReadyAt: 100, Version: 1,
	}
	ss, cancel := openSession(t, fs, pl, "gs-1")
	defer cancel()

	ss.send(&gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Counters{Counters: &gatewayv1.CountersSync{
			Counters: map[string]*gatewayv1.CounterState{"players": {Count: 4, Capacity: 8}},
			Lists:    map[string]*gatewayv1.ListState{"sessions": {Capacity: 2, Values: []string{"x"}}},
		}},
	})

	ctx := context.Background()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snaps, err := pl.Counters(ctx, []string{"gs-1"})
		if err != nil {
			t.Fatal(err)
		}
		if snap, ok := snaps["gs-1"]; ok {
			if snap.Counters["players"] != (pool.Counter{Count: 4, Capacity: 8}) {
				t.Fatalf("players = %+v", snap.Counters["players"])
			}
			if l := snap.Lists["sessions"]; l.Capacity != 2 || len(l.Values) != 1 || l.Values[0] != "x" {
				t.Fatalf("sessions list = %+v", l)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("counters snapshot never persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
