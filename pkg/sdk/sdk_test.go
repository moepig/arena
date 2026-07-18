package sdk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// fakeSDK records calls and serves canned state.
type fakeSDK struct {
	arenav1connect.UnimplementedSDKHandler
	mu     sync.Mutex
	calls  []string
	labels map[string]string
}

func (f *fakeSDK) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeSDK) Ready(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	f.record("Ready")
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (f *fakeSDK) Health(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	f.record("Health")
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (f *fakeSDK) GetGameServer(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[arenav1.GameServer], error) {
	return connect.NewResponse(&arenav1.GameServer{Id: "gs-1", Address: "203.0.113.24"}), nil
}

func (f *fakeSDK) SetLabel(_ context.Context, req *connect.Request[arenav1.KeyValue]) (*connect.Response[emptypb.Empty], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labels == nil {
		f.labels = map[string]string{}
	}
	f.labels[req.Msg.GetKey()] = req.Msg.GetValue()
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (f *fakeSDK) WatchGameServer(_ context.Context, _ *connect.Request[emptypb.Empty], stream *connect.ServerStream[arenav1.GameServer]) error {
	for _, state := range []arenav1.GameServer_State{
		arenav1.GameServer_STATE_READY,
		arenav1.GameServer_STATE_ALLOCATED,
	} {
		if err := stream.Send(&arenav1.GameServer{Id: "gs-1", State: state}); err != nil {
			return err
		}
	}
	return nil
}

func startFake(t *testing.T) (*fakeSDK, *Client) {
	t.Helper()
	fs := &fakeSDK{}
	mux := http.NewServeMux()
	mux.Handle(arenav1connect.NewSDKHandler(fs))
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)
	return fs, NewForAddress(srv.URL)
}

func TestClientCallsSidecar(t *testing.T) {
	fs, c := startFake(t)
	ctx := context.Background()

	if err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLabel(ctx, "version", "v1"); err != nil {
		t.Fatal(err)
	}

	gs, err := c.GameServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gs.GetAddress() != "203.0.113.24" {
		t.Errorf("address = %q", gs.GetAddress())
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.calls) != 2 || fs.calls[0] != "Ready" || fs.calls[1] != "Health" {
		t.Errorf("calls = %v", fs.calls)
	}
	if fs.labels["version"] != "v1" {
		t.Errorf("labels = %v", fs.labels)
	}
}

func TestWatchGameServer(t *testing.T) {
	_, c := startFake(t)

	var states []arenav1.GameServer_State
	err := c.WatchGameServer(context.Background(), func(gs *arenav1.GameServer) {
		states = append(states, gs.GetState())
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[1] != arenav1.GameServer_STATE_ALLOCATED {
		t.Errorf("states = %v", states)
	}
}

func TestNewUsesEnvAddress(t *testing.T) {
	t.Setenv(addressEnv, "http://127.0.0.1:1")
	if c := New(); c == nil {
		t.Fatal("New returned nil")
	}
}
