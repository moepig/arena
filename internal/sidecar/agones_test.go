package sidecar

// Agones wire-compat surface: gRPC service adaptation and
// the REST (grpc-gateway equivalent) routes.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	sdk "github.com/moepig/arena/gen/agones/dev/sdk"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

func TestAgonesServerForwardsCalls(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1"}, slog.Default())
	srv := NewAgonesServer(sc)
	ctx := context.Background()

	if _, err := srv.Ready(ctx, connect.NewRequest(&sdk.Empty{})); err != nil {
		t.Fatal(err)
	}
	if msg := <-sc.outbox; msg.GetReady() == nil {
		t.Errorf("outbox after Ready = %v, want ready{}", msg)
	}

	if _, err := srv.Allocate(ctx, connect.NewRequest(&sdk.Empty{})); err != nil {
		t.Fatal(err)
	}
	if msg := <-sc.outbox; msg.GetAllocate() == nil {
		t.Errorf("outbox after Allocate = %v, want allocate{}", msg)
	}

	if _, err := srv.Reserve(ctx, connect.NewRequest(&sdk.Duration{Seconds: 30})); err != nil {
		t.Fatal(err)
	}
	if msg := <-sc.outbox; msg.GetReserve().GetSeconds() != 30 {
		t.Errorf("outbox after Reserve = %v, want reserve{seconds:30}", msg)
	}

	if _, err := srv.SetLabel(ctx, connect.NewRequest(&sdk.KeyValue{Key: "k", Value: "v"})); err != nil {
		t.Fatal(err)
	}
	if msg := <-sc.outbox; msg.GetSetMetadata().GetKey() != "k" {
		t.Errorf("outbox after SetLabel = %v, want set_metadata{k}", msg)
	}
}

func TestToAgonesGameServerMapping(t *testing.T) {
	gs := &arenav1.GameServer{
		Id:        "gs-1",
		Name:      "fleet-a-gs1",
		Namespace: "default",
		FleetId:   "f-1",
		State:     arenav1.GameServer_STATE_ALLOCATED,
		Address:   "203.0.113.5",
		Ports:     []*arenav1.Port{{Name: "game", Port: 7777}},
		Labels:    map[string]string{"mode": "ranked"},
		CreatedAt: 1234,
	}
	out := ToAgonesGameServer(gs)
	if out.GetObjectMeta().GetName() != "fleet-a-gs1" || out.GetObjectMeta().GetUid() != "gs-1" {
		t.Errorf("object_meta = %v", out.GetObjectMeta())
	}
	if out.GetObjectMeta().GetAnnotations()["arena.dev/fleet-id"] != "f-1" {
		t.Errorf("annotations = %v, want arena.dev/fleet-id", out.GetObjectMeta().GetAnnotations())
	}
	if out.GetStatus().GetState() != "Allocated" || out.GetStatus().GetAddress() != "203.0.113.5" {
		t.Errorf("status = %v", out.GetStatus())
	}
	if len(out.GetStatus().GetPorts()) != 1 || out.GetStatus().GetPorts()[0].GetPort() != 7777 {
		t.Errorf("ports = %v", out.GetStatus().GetPorts())
	}

	// Lifecycle collapse for states Agones does not have.
	for _, tc := range []struct {
		in   arenav1.GameServer_State
		want string
	}{
		{arenav1.GameServer_STATE_STARTING, "Scheduled"},
		{arenav1.GameServer_STATE_DRAINING, "Shutdown"},
		{arenav1.GameServer_STATE_RESERVED, "Reserved"},
	} {
		if got := agonesState(tc.in); got != tc.want {
			t.Errorf("agonesState(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAgonesRESTEndpoints(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1"}, slog.Default())
	sc.setState(&arenav1.GameServer{Id: "gs-1", State: arenav1.GameServer_STATE_READY, Address: "10.0.0.1"})
	srv := httptest.NewServer(NewAgonesRESTHandler(sc))
	defer srv.Close()

	post := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := post("/ready", "{}"); res.StatusCode != http.StatusOK {
		t.Fatalf("POST /ready = %d", res.StatusCode)
	}
	if msg := <-sc.outbox; msg.GetReady() == nil {
		t.Error("REST /ready did not queue a ready message")
	}

	if res := post("/reserve", `{"seconds":"15"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("POST /reserve = %d", res.StatusCode)
	}
	if msg := <-sc.outbox; msg.GetReserve().GetSeconds() != 15 {
		t.Error("REST /reserve did not queue the right reservation")
	}

	if res := post("/health", "{}"); res.StatusCode != http.StatusOK {
		t.Fatalf("POST /health = %d", res.StatusCode)
	}
	if sc.lastHealth.Load() == 0 {
		t.Error("REST /health did not record a health ping")
	}

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/metadata/label", strings.NewReader(`{"key":"k","value":"v"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("PUT /metadata/label = %v %v", res, err)
	}
	if msg := <-sc.outbox; msg.GetSetMetadata().GetKey() != "k" {
		t.Error("REST label did not queue set_metadata")
	}

	gres, err := http.Get(srv.URL + "/gameserver")
	if err != nil || gres.StatusCode != http.StatusOK {
		t.Fatalf("GET /gameserver = %v %v", gres, err)
	}
	var body struct {
		Status struct {
			State   string `json:"state"`
			Address string `json:"address"`
		} `json:"status"`
	}
	if err := json.NewDecoder(gres.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status.State != "Ready" || body.Status.Address != "10.0.0.1" {
		t.Errorf("GET /gameserver status = %+v", body.Status)
	}
}

func TestAgonesRESTWatchStreams(t *testing.T) {
	sc := New(nil, Options{GameServerID: "gs-1"}, slog.Default())
	srv := httptest.NewServer(NewAgonesRESTHandler(sc))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/watch/gameserver", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	go sc.setState(&arenav1.GameServer{Id: "gs-1", State: arenav1.GameServer_STATE_ALLOCATED})

	dec := json.NewDecoder(res.Body)
	var line struct {
		Result struct {
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"result"`
	}
	if err := dec.Decode(&line); err != nil {
		t.Fatal(err)
	}
	if line.Result.Status.State != "Allocated" {
		t.Errorf("watch line = %+v, want Allocated", line)
	}
}
