// Package api hosts the connect handlers for FleetService, GameServerService
// and AllocationService. One handler serves gRPC, gRPC-Web and
// Connect JSON on a single port.
package api

import (
	"errors"
	"net/http"

	"connectrpc.com/connect"

	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/store"
)

// NewMux returns an http.Handler with all arena-api services mounted.
// opts (e.g. the authn/authz interceptors) apply to the
// control-plane services only; the SDK Gateway is mounted separately with
// its own identity scheme.
func NewMux(s *store.Store, alloc *allocation.Allocator, opts ...connect.HandlerOption) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(arenav1connect.NewFleetServiceHandler(&FleetServer{store: s}, opts...))
	mux.Handle(arenav1connect.NewGameServerServiceHandler(&GameServerServer{store: s}, opts...))
	mux.Handle(arenav1connect.NewAllocationServiceHandler(&AllocationServer{store: s, allocator: alloc}, opts...))
	mux.Handle(arenav1connect.NewEventServiceHandler(&EventServer{store: s}, opts...))
	return mux
}

// asConnectError maps store sentinel errors to the API error model.
// Unknown errors pass through as internal.
func asConnectError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrVersionConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, store.ErrConditionFailed):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return err
	}
}
