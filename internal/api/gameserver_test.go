package api

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

// fakeGSStore is an in-memory GameServerStore.
type fakeGSStore struct {
	gss map[string]*store.GameServer
}

func (f *fakeGSStore) GetGameServer(_ context.Context, id string) (*store.GameServer, error) {
	if gs, ok := f.gss[id]; ok {
		cp := *gs
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeGSStore) GetFleetByName(context.Context, string, string) (*store.Fleet, error) {
	return nil, store.ErrNotFound
}

func (f *fakeGSStore) ListGameServersByFleet(context.Context, string, store.State, int32, string) ([]store.GameServer, string, error) {
	return nil, "", nil
}

func (f *fakeGSStore) TransitionState(_ context.Context, id string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gss[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	if !store.CanTransition(from, to) || gs.State != from {
		return nil, fmt.Errorf("%w: %s -> %s from %s", store.ErrConditionFailed, from, to, gs.State)
	}
	gs.State = to
	gs.Version++
	if mutate != nil {
		mutate(gs)
	}
	cp := *gs
	return &cp, nil
}

func TestDeleteGameServer(t *testing.T) {
	cases := []struct {
		from store.State
		want store.State // resulting state (== from for terminal no-ops)
	}{
		{store.StateReady, store.StateDraining},
		{store.StateAllocated, store.StateDraining},
		{store.StateReserved, store.StateDraining},
		{store.StateScheduled, store.StateUnhealthy},
		{store.StateStarting, store.StateUnhealthy},
		{store.StateDraining, store.StateDraining},
		{store.StateUnhealthy, store.StateUnhealthy},
		{store.StateTerminated, store.StateTerminated},
	}
	for _, tc := range cases {
		t.Run(string(tc.from), func(t *testing.T) {
			fs := &fakeGSStore{gss: map[string]*store.GameServer{
				"gs-1": {ID: "gs-1", FleetID: "f1", State: tc.from, Version: 1},
			}}
			srv := &GameServerServer{store: fs}
			res, err := srv.DeleteGameServer(context.Background(),
				connect.NewRequest(&arenav1.DeleteGameServerRequest{Id: "gs-1"}))
			if err != nil {
				t.Fatal(err)
			}
			if got := fs.gss["gs-1"].State; got != tc.want {
				t.Errorf("state = %s, want %s", got, tc.want)
			}
			if res.Msg.GetId() != "gs-1" {
				t.Errorf("response id = %q, want gs-1", res.Msg.GetId())
			}
		})
	}
}

func TestDeleteGameServerValidation(t *testing.T) {
	srv := &GameServerServer{store: &fakeGSStore{gss: map[string]*store.GameServer{}}}
	_, err := srv.DeleteGameServer(context.Background(),
		connect.NewRequest(&arenav1.DeleteGameServerRequest{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty id err = %v, want INVALID_ARGUMENT", err)
	}
	_, err = srv.DeleteGameServer(context.Background(),
		connect.NewRequest(&arenav1.DeleteGameServerRequest{Id: "nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("missing gs err = %v, want NOT_FOUND", err)
	}
}
