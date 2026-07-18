package sidecar

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// SDKServer serves the Agones-compatible SDK (arena/v1/sdk.proto) on
// localhost to the game server container, backed by the Sidecar's gateway
// session. Requests are acknowledged once queued upstream; the observable
// result arrives as a state push (GetGameServer / WatchGameServer), matching
// the Agones SDK's asynchronous semantics.
type SDKServer struct {
	arenav1connect.UnimplementedSDKHandler
	sc *Sidecar
}

// NewSDKServer returns the local SDK service for a Sidecar.
func NewSDKServer(sc *Sidecar) *SDKServer { return &SDKServer{sc: sc} }

var _ arenav1connect.SDKHandler = (*SDKServer)(nil)

// Ready requests the Starting → Ready transition.
func (s *SDKServer) Ready(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	err := s.sc.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// Health marks the game process healthy; the sidecar's heartbeat loop keeps
// reporting liveness upstream while these calls keep coming.
func (s *SDKServer) Health(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	s.sc.recordHealth()
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// Shutdown signals graceful shutdown; the controller stops the task.
func (s *SDKServer) Shutdown(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	err := s.sc.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Shutdown{Shutdown: &gatewayv1.ShutdownRequest{}},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// GetGameServer returns the last state pushed by the gateway.
func (s *SDKServer) GetGameServer(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[arenav1.GameServer], error) {
	gs := s.sc.State()
	if gs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("gameserver state not received yet"))
	}
	return connect.NewResponse(gs), nil
}

// SetLabel sets a metadata label.
func (s *SDKServer) SetLabel(ctx context.Context, req *connect.Request[arenav1.KeyValue]) (*connect.Response[emptypb.Empty], error) {
	return s.setMetadata(ctx, gatewayv1.SetMetadataRequest_KIND_LABEL, req.Msg)
}

// SetAnnotation sets a metadata annotation.
func (s *SDKServer) SetAnnotation(ctx context.Context, req *connect.Request[arenav1.KeyValue]) (*connect.Response[emptypb.Empty], error) {
	return s.setMetadata(ctx, gatewayv1.SetMetadataRequest_KIND_ANNOTATION, req.Msg)
}

func (s *SDKServer) setMetadata(ctx context.Context, kind gatewayv1.SetMetadataRequest_Kind, kv *arenav1.KeyValue) (*connect.Response[emptypb.Empty], error) {
	if kv.GetKey() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("key is required"))
	}
	err := s.sc.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_SetMetadata{SetMetadata: &gatewayv1.SetMetadataRequest{
			Kind:  kind,
			Key:   kv.GetKey(),
			Value: kv.GetValue(),
		}},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// Reserve moves the server out of the ready pool for the given duration
// (zero = until Ready/Allocate/Shutdown). Sub-second
// durations round up to one second: the wire carries whole seconds, and
// silently reserving forever (0) would invert the caller's intent.
func (s *SDKServer) Reserve(ctx context.Context, req *connect.Request[durationpb.Duration]) (*connect.Response[emptypb.Empty], error) {
	seconds, err := reserveSeconds(req.Msg.AsDuration())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	err = s.sc.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Reserve{Reserve: &gatewayv1.ReserveRequest{Seconds: seconds}},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// Allocate self-allocates the server from Ready/Reserved.
func (s *SDKServer) Allocate(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	err := s.sc.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Allocate{Allocate: &gatewayv1.SelfAllocateRequest{}},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// WatchGameServer streams state updates (allocation pushes included) until
// the caller goes away.
func (s *SDKServer) WatchGameServer(ctx context.Context, _ *connect.Request[emptypb.Empty], stream *connect.ServerStream[arenav1.GameServer]) error {
	id, ch := s.sc.watch()
	defer s.sc.unwatch(id)
	for {
		select {
		case <-ctx.Done():
			return nil
		case gs := <-ch:
			if err := stream.Send(gs); err != nil {
				return err
			}
		}
	}
}
