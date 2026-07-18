package sidecar

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// fakeGateway records incoming sidecar messages and lets tests push state.
type fakeGateway struct {
	mu       sync.Mutex
	sessions int
	received []*gatewayv1.SidecarMessage
	sends    chan *gatewayv1.GatewayMessage
	// dropAfterHello makes each session fail right after the first message
	// (reconnect testing).
	dropAfterHello bool
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{sends: make(chan *gatewayv1.GatewayMessage, 16)}
}

func (f *fakeGateway) Session(ctx context.Context, stream *connect.BidiStream[gatewayv1.SidecarMessage, gatewayv1.GatewayMessage]) error {
	f.mu.Lock()
	f.sessions++
	drop := f.dropAfterHello
	f.mu.Unlock()

	first, err := stream.Receive()
	if err != nil {
		return err
	}
	f.record(first)
	if drop {
		return connect.NewError(connect.CodeUnavailable, nil)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-f.sends:
				if err := stream.Send(msg); err != nil {
					return
				}
			}
		}
	}()
	for {
		msg, err := stream.Receive()
		if err != nil {
			return err
		}
		f.record(msg)
	}
}

func (f *fakeGateway) record(m *gatewayv1.SidecarMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, m)
}

func (f *fakeGateway) messages() []*gatewayv1.SidecarMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*gatewayv1.SidecarMessage(nil), f.received...)
}

func (f *fakeGateway) sessionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions
}

// pushState emulates a gateway state push (allocation notification etc.).
func (f *fakeGateway) pushState(gs *arenav1.GameServer) {
	f.sends <- &gatewayv1.GatewayMessage{Msg: &gatewayv1.GatewayMessage_State{State: gs}}
}

func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// startSidecar wires a Sidecar to a fake gateway over a real h2c stream.
func startSidecar(t *testing.T, fg *fakeGateway, opts Options) (*Sidecar, context.CancelFunc) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(gatewayv1connect.NewSDKGatewayHandler(fg))
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)

	if opts.GameServerID == "" {
		opts.GameServerID = "gs-1"
	}
	if opts.MinBackoff == 0 {
		opts.MinBackoff = 10 * time.Millisecond
	}
	sc := New(gatewayv1connect.NewSDKGatewayClient(h2cClient(), srv.URL), opts, slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = sc.Run(ctx) }()
	t.Cleanup(cancel)
	return sc, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSessionHelloCarriesIdentity(t *testing.T) {
	fg := newFakeGateway()
	startSidecar(t, fg, Options{GameServerID: "gs-42", TaskARN: "arn:task/42"})

	waitFor(t, "hello", func() bool { return len(fg.messages()) >= 1 })
	hello := fg.messages()[0]
	if hello.GetGameserverId() != "gs-42" || hello.GetTaskArn() != "arn:task/42" {
		t.Errorf("hello = (%q, %q), want (gs-42, arn:task/42)", hello.GetGameserverId(), hello.GetTaskArn())
	}
}

func TestReadyForwardedAndStateCached(t *testing.T) {
	fg := newFakeGateway()
	sc, _ := startSidecar(t, fg, Options{})
	sdk := NewSDKServer(sc)

	waitFor(t, "session", func() bool { return fg.sessionCount() >= 1 })
	if _, err := sdk.Ready(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "ready upstream", func() bool {
		for _, m := range fg.messages() {
			if m.GetReady() != nil {
				return true
			}
		}
		return false
	})

	fg.pushState(&arenav1.GameServer{Id: "gs-1", State: arenav1.GameServer_STATE_READY})
	waitFor(t, "state cache", func() bool { return sc.State() != nil })

	res, err := sdk.GetGameServer(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetState() != arenav1.GameServer_STATE_READY {
		t.Errorf("cached state = %v, want READY", res.Msg.GetState())
	}
}

func TestGetGameServerBeforeFirstPush(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1"}, slog.Default())
	_, err := NewSDKServer(sc).GetGameServer(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}
}

func TestWatchReceivesPushes(t *testing.T) {
	fg := newFakeGateway()
	sc, _ := startSidecar(t, fg, Options{})

	waitFor(t, "session", func() bool { return fg.sessionCount() >= 1 })
	id, ch := sc.watch()
	defer sc.unwatch(id)

	fg.pushState(&arenav1.GameServer{Id: "gs-1", State: arenav1.GameServer_STATE_ALLOCATED})
	select {
	case gs := <-ch:
		if gs.GetState() != arenav1.GameServer_STATE_ALLOCATED {
			t.Errorf("watched state = %v, want ALLOCATED", gs.GetState())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch never received the push")
	}
}

func TestHeartbeatsFlow(t *testing.T) {
	fg := newFakeGateway()
	startSidecar(t, fg, Options{HeartbeatInterval: 10 * time.Millisecond})

	waitFor(t, "heartbeats", func() bool {
		n := 0
		for _, m := range fg.messages() {
			if m.GetHeartbeat() != nil {
				n++
			}
		}
		return n >= 3
	})
}

func TestReconnectWithBackoff(t *testing.T) {
	fg := newFakeGateway()
	fg.dropAfterHello = true
	startSidecar(t, fg, Options{})

	waitFor(t, "reconnects", func() bool { return fg.sessionCount() >= 3 })
}

func TestHealthGate(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1", HealthTimeout: 30 * time.Second}, slog.Default())
	base := time.Unix(1_000, 0)
	sc.now = func() time.Time { return base }

	if !sc.healthy() {
		t.Error("no Health() yet: heartbeats must flow (startup grace)")
	}
	sc.recordHealth()
	sc.now = func() time.Time { return base.Add(10 * time.Second) }
	if !sc.healthy() {
		t.Error("recent Health(): must be healthy")
	}
	sc.now = func() time.Time { return base.Add(31 * time.Second) }
	if sc.healthy() {
		t.Error("Health() stopped past the timeout: heartbeats must stop")
	}
}

// TestReserveAndAllocateQueueUpstream verifies the Reserve and Allocate SDK
// calls turn into the right gateway messages on the outbox.
func TestReserveAndAllocateQueueUpstream(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1"}, slog.Default())
	sdk := NewSDKServer(sc)

	if _, err := sdk.Reserve(context.Background(), connect.NewRequest(durationpb.New(-time.Second))); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("Reserve(-1s) err = %v, want INVALID_ARGUMENT", err)
	}

	if _, err := sdk.Reserve(context.Background(), connect.NewRequest(durationpb.New(90*time.Second))); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	msg := <-sc.outbox
	r := msg.GetReserve()
	if r == nil || r.GetSeconds() != 90 {
		t.Errorf("outbox after Reserve = %v, want reserve{seconds:90}", msg)
	}

	if _, err := sdk.Allocate(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	msg = <-sc.outbox
	if msg.GetAllocate() == nil {
		t.Errorf("outbox after Allocate = %v, want allocate{}", msg)
	}
}

// Compile-time: the SDK server satisfies the full Agones-compatible surface.
var _ arenav1connect.SDKHandler = (*SDKServer)(nil)
