package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/store"
)

// GameServerStore is the store surface GameServerService needs
// (*store.Store satisfies it; tests substitute an in-memory fake).
type GameServerStore interface {
	GetGameServer(ctx context.Context, gsID string) (*store.GameServer, error)
	GetFleetByName(ctx context.Context, namespace, name string) (*store.Fleet, error)
	ListGameServersByFleet(ctx context.Context, fleetID string, state store.State, pageSize int32, pageToken string) ([]store.GameServer, string, error)
	TransitionState(ctx context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error)
}

// GameServerServer implements arena.v1.GameServerService.
type GameServerServer struct {
	arenav1connect.UnimplementedGameServerServiceHandler
	store GameServerStore
}

func (s *GameServerServer) GetGameServer(ctx context.Context, req *connect.Request[arenav1.GetGameServerRequest]) (*connect.Response[arenav1.GameServer], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	gs, err := s.store.GetGameServer(ctx, req.Msg.GetId())
	if err != nil {
		return nil, asConnectError(err)
	}
	return connect.NewResponse(convert.GameServerToProto(gs)), nil
}

// DeleteGameServer requests removal of a single GameServer,
// reusing the existing teardown flows: live servers go Draining
// (graceful stop; the fleet reconciler replaces them), pre-Ready servers go
// Unhealthy, and already-terminal states are an idempotent no-op. Redis is
// not touched — a stale pool entry is rejected by the conditional claim.
func (s *GameServerServer) DeleteGameServer(ctx context.Context, req *connect.Request[arenav1.DeleteGameServerRequest]) (*connect.Response[arenav1.GameServer], error) {
	if req.Msg.GetId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	gs, err := s.store.GetGameServer(ctx, req.Msg.GetId())
	if err != nil {
		return nil, asConnectError(err)
	}

	var to store.State
	switch gs.State {
	case store.StateReady, store.StateAllocated, store.StateReserved:
		to = store.StateDraining
	case store.StateScheduled, store.StateStarting:
		to = store.StateUnhealthy
	default: // Draining / Unhealthy / Terminated: already on the way out
		return connect.NewResponse(convert.GameServerToProto(gs)), nil
	}

	updated, err := s.store.TransitionState(ctx, gs.ID, gs.State, to, nil)
	if errors.Is(err, store.ErrConditionFailed) {
		// Concurrent transition — report where the server actually is; the
		// winner owns the teardown.
		cur, gerr := s.store.GetGameServer(ctx, gs.ID)
		if gerr != nil {
			return nil, asConnectError(gerr)
		}
		return connect.NewResponse(convert.GameServerToProto(cur)), nil
	}
	if err != nil {
		return nil, asConnectError(err)
	}
	return connect.NewResponse(convert.GameServerToProto(updated)), nil
}

func (s *GameServerServer) ListGameServers(ctx context.Context, req *connect.Request[arenav1.ListGameServersRequest]) (*connect.Response[arenav1.ListGameServersResponse], error) {
	m := req.Msg
	// Listing rides the fleet-index GSI, so a fleet is required. A
	// namespace-wide listing would need a scan; not offered.
	if m.GetFleetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("fleet_name is required"))
	}
	fleet, err := s.store.GetFleetByName(ctx, namespaceOrDefault(m.GetNamespace()), m.GetFleetName())
	if err != nil {
		return nil, asConnectError(err)
	}

	size := m.GetPageSize()
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	gss, next, err := s.store.ListGameServersByFleet(ctx, fleet.ID, convert.StateFromProto(m.GetState()), size, m.GetPageToken())
	if err != nil {
		return nil, asConnectError(err)
	}
	resp := &arenav1.ListGameServersResponse{NextPageToken: next}
	for i := range gss {
		resp.GameServers = append(resp.GameServers, convert.GameServerToProto(&gss[i]))
	}
	return connect.NewResponse(resp), nil
}
