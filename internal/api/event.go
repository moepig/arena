package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/store"
)

const maxEventLimit = 200

// EventStore is the store surface EventService needs.
type EventStore interface {
	ListEvents(ctx context.Context, resourceType, resourceID string, limit int32) ([]store.Event, error)
}

// EventServer implements arena.v1.EventService.
type EventServer struct {
	arenav1connect.UnimplementedEventServiceHandler
	store EventStore
}

func (s *EventServer) ListEvents(ctx context.Context, req *connect.Request[arenav1.ListEventsRequest]) (*connect.Response[arenav1.ListEventsResponse], error) {
	m := req.Msg
	if m.GetResourceType() != store.EventResourceFleet && m.GetResourceType() != store.EventResourceGameServer {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New(`resource_type must be "fleet" or "gameserver"`))
	}
	if m.GetResourceId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("resource_id is required"))
	}
	limit := m.GetLimit()
	if limit > maxEventLimit {
		limit = maxEventLimit
	}
	events, err := s.store.ListEvents(ctx, m.GetResourceType(), m.GetResourceId(), limit)
	if err != nil {
		return nil, asConnectError(err)
	}
	resp := &arenav1.ListEventsResponse{}
	for _, ev := range events {
		resp.Events = append(resp.Events, &arenav1.Event{
			ResourceType: ev.ResourceType,
			ResourceId:   ev.ResourceID,
			Timestamp:    ev.TS / 1e9,
			Type:         ev.Type,
			Reason:       ev.Reason,
			Message:      ev.Message,
		})
	}
	return connect.NewResponse(resp), nil
}
