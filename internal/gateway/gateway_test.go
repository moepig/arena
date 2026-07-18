package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"connectrpc.com/connect"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// fakeStore is a minimal in-memory Store for gateway tests.
type fakeStore struct {
	mu       sync.Mutex
	gss      map[string]*store.GameServer
	allocs   []store.Allocation
	released []string // gameserver IDs passed to ReleaseActiveAllocationsForGameServer
}

func (f *fakeStore) GetGameServer(_ context.Context, id string) (*store.GameServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if gs, ok := f.gss[id]; ok {
		cp := *gs
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) TransitionState(_ context.Context, id string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gs, ok := f.gss[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != from {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	gs.State = to
	gs.Version++
	if to == store.StateReady {
		gs.ReadyAt = time.Now().Unix()
	}
	if mutate != nil {
		mutate(gs)
	}
	cp := *gs
	return &cp, nil
}

func (f *fakeStore) UpdateGameServerMetadata(_ context.Context, id string, mutate func(*store.GameServer)) (*store.GameServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gs, ok := f.gss[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	mutate(gs)
	gs.Version++
	cp := *gs
	return &cp, nil
}

func (f *fakeStore) SelfAllocateGameServer(_ context.Context, id string, alloc store.Allocation) (*store.GameServer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gs, ok := f.gss[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State != store.StateReady && gs.State != store.StateReserved {
		return nil, fmt.Errorf("%w: state %s", store.ErrConditionFailed, gs.State)
	}
	gs.State = store.StateAllocated
	gs.AllocatedAt = time.Now().Unix()
	gs.ReservedUntil = 0
	gs.Version++
	alloc.GameServerID = id
	alloc.FleetID = gs.FleetID
	f.allocs = append(f.allocs, alloc)
	cp := *gs
	return &cp, nil
}

func (f *fakeStore) ReleaseActiveAllocationsForGameServer(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	return nil
}

// startGateway serves the SDKGateway over h2c and returns a connect client.
func startGateway(t *testing.T, fs *fakeStore, pl *pool.Pool) gatewayv1connect.SDKGatewayClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(gatewayv1connect.NewSDKGatewayHandler(New(fs, pl, nil, nil)))
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	client := &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
	return gatewayv1connect.NewSDKGatewayClient(client, srv.URL, connect.WithGRPC())
}

func newTestDeps(t *testing.T) (*fakeStore, *pool.Pool) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	pl := pool.New(rdb)
	if err := pl.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fakeStore{gss: map[string]*store.GameServer{}}, pl
}

// receiveState reads messages until a state push arrives (skipping acks).
func receiveState(t *testing.T, stream *connect.BidiStreamForClient[gatewayv1.SidecarMessage, gatewayv1.GatewayMessage]) *arenav1.GameServer {
	t.Helper()
	for i := 0; i < 10; i++ {
		msg, err := stream.Receive()
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		if st := msg.GetState(); st != nil {
			return st
		}
	}
	t.Fatal("no state push within 10 messages")
	return nil
}

func TestSessionReadyFlow(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateStarting, Version: 1,
	}
	client := startGateway(t, fs, pl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Session(ctx)

	// Hello: declares identity; gateway replies with current state.
	if err := stream.Send(&gatewayv1.SidecarMessage{GameserverId: "gs-1", TaskArn: "arn:aws:ecs:task/x"}); err != nil {
		t.Fatal(err)
	}
	if st := receiveState(t, stream); st.GetState() != arenav1.GameServer_STATE_STARTING {
		t.Fatalf("initial state = %v, want STARTING", st.GetState())
	}

	// Ready() commits the transition, then pools the server.
	if err := stream.Send(&gatewayv1.SidecarMessage{
		GameserverId: "gs-1",
		Msg:          &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}},
	}); err != nil {
		t.Fatal(err)
	}
	if st := receiveState(t, stream); st.GetState() != arenav1.GameServer_STATE_READY {
		t.Fatalf("post-Ready state = %v, want READY", st.GetState())
	}
	if got, _ := pl.PopMin(ctx, "f1"); got != "gs-1" {
		t.Fatalf("pool candidate = %q, want gs-1", got)
	}

	// Heartbeat lands in Redis with TTL.
	if err := stream.Send(&gatewayv1.SidecarMessage{
		GameserverId: "gs-1",
		Msg:          &gatewayv1.SidecarMessage_Heartbeat{Heartbeat: &gatewayv1.Heartbeat{}},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		alive, err := pl.Heartbeats(ctx, []string{"gs-1"})
		if err != nil {
			t.Fatal(err)
		}
		if alive[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("heartbeat never landed in Redis")
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = stream.CloseRequest()
}

func TestSessionAllocationPush(t *testing.T) {
	fs, pl := newTestDeps(t)
	fs.gss["gs-1"] = &store.GameServer{
		ID: "gs-1", FleetID: "f1", State: store.StateReady, Version: 1,
	}
	client := startGateway(t, fs, pl)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Session(ctx)
	if err := stream.Send(&gatewayv1.SidecarMessage{GameserverId: "gs-1"}); err != nil {
		t.Fatal(err)
	}
	receiveState(t, stream) // initial state

	// Simulate the allocator's push (pub/sub → stream).
	allocated := &store.GameServer{ID: "gs-1", FleetID: "f1", State: store.StateAllocated, Address: "203.0.113.5"}
	// Retry: the pushLoop subscription may not be established yet.
	go func() {
		for i := 0; i < 50; i++ {
			_ = pl.PublishAllocation(ctx, "gs-1", convert.EncodeStatePush(allocated))
			time.Sleep(50 * time.Millisecond)
		}
	}()

	st := receiveState(t, stream)
	if st.GetState() != arenav1.GameServer_STATE_ALLOCATED || st.GetAddress() != "203.0.113.5" {
		t.Fatalf("push state = %v addr %q, want ALLOCATED / 203.0.113.5", st.GetState(), st.GetAddress())
	}
	_ = stream.CloseRequest()
}

func TestSessionRejectsMissingID(t *testing.T) {
	fs, pl := newTestDeps(t)
	client := startGateway(t, fs, pl)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.Session(ctx)
	if err := stream.Send(&gatewayv1.SidecarMessage{}); err != nil {
		t.Fatal(err)
	}
	_, err := stream.Receive()
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err = %v, want INVALID_ARGUMENT", err)
	}
}
