package sidecar

// Agones wire-compatible SDK surface: the sidecar serves
// the real agones.dev.sdk.SDK service so official Agones client SDKs
// (Unity / Unreal / C# / C++ / Rust / Node) work unmodified. Calls adapt to
// the same SidecarMessage stream the arena SDK uses; GameServer objects map
// onto the Agones message shape.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	sdk "github.com/moepig/arena/gen/agones/dev/sdk"
	"github.com/moepig/arena/gen/agones/dev/sdk/sdkconnect"
	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

// AgonesServer implements agones.dev.sdk.SDK backed by a Sidecar.
type AgonesServer struct {
	sdkconnect.UnimplementedSDKHandler
	sc *Sidecar
}

// NewAgonesServer returns the Agones wire-compatible SDK service.
func NewAgonesServer(sc *Sidecar) *AgonesServer { return &AgonesServer{sc: sc} }

var _ sdkconnect.SDKHandler = (*AgonesServer)(nil)

func (a *AgonesServer) Ready(ctx context.Context, _ *connect.Request[sdk.Empty]) (*connect.Response[sdk.Empty], error) {
	return a.forward(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}},
	})
}

func (a *AgonesServer) Allocate(ctx context.Context, _ *connect.Request[sdk.Empty]) (*connect.Response[sdk.Empty], error) {
	return a.forward(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Allocate{Allocate: &gatewayv1.SelfAllocateRequest{}},
	})
}

func (a *AgonesServer) Shutdown(ctx context.Context, _ *connect.Request[sdk.Empty]) (*connect.Response[sdk.Empty], error) {
	return a.forward(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Shutdown{Shutdown: &gatewayv1.ShutdownRequest{}},
	})
}

// Health is client-streaming in Agones: every received Empty is one health
// ping. The stream stays open for the lifetime of the game process.
func (a *AgonesServer) Health(ctx context.Context, stream *connect.ClientStream[sdk.Empty]) (*connect.Response[sdk.Empty], error) {
	for stream.Receive() {
		a.sc.recordHealth()
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return nil, err
	}
	return connect.NewResponse(&sdk.Empty{}), nil
}

func (a *AgonesServer) GetGameServer(_ context.Context, _ *connect.Request[sdk.Empty]) (*connect.Response[sdk.GameServer], error) {
	gs := a.sc.State()
	if gs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("gameserver state not received yet"))
	}
	return connect.NewResponse(a.withCounters(ToAgonesGameServer(gs))), nil
}

// withCounters injects the live local Counter/List state into the status —
// the sidecar copy is the primary source.
func (a *AgonesServer) withCounters(out *sdk.GameServer) *sdk.GameServer {
	counters, lists := a.sc.CounterSnapshot()
	if len(counters) > 0 {
		out.Status.Counters = make(map[string]*sdk.GameServer_Status_CounterStatus, len(counters))
		for name, c := range counters {
			out.Status.Counters[name] = &sdk.GameServer_Status_CounterStatus{Count: c.Count, Capacity: c.Capacity}
		}
	}
	if len(lists) > 0 {
		out.Status.Lists = make(map[string]*sdk.GameServer_Status_ListStatus, len(lists))
		for name, l := range lists {
			out.Status.Lists[name] = &sdk.GameServer_Status_ListStatus{Capacity: l.Capacity, Values: l.Values}
		}
	}
	return out
}

func (a *AgonesServer) WatchGameServer(ctx context.Context, _ *connect.Request[sdk.Empty], stream *connect.ServerStream[sdk.GameServer]) error {
	id, ch := a.sc.watch()
	defer a.sc.unwatch(id)
	for {
		select {
		case <-ctx.Done():
			return nil
		case gs := <-ch:
			if err := stream.Send(a.withCounters(ToAgonesGameServer(gs))); err != nil {
				return err
			}
		}
	}
}

func (a *AgonesServer) SetLabel(ctx context.Context, req *connect.Request[sdk.KeyValue]) (*connect.Response[sdk.Empty], error) {
	return a.setMetadata(ctx, gatewayv1.SetMetadataRequest_KIND_LABEL, req.Msg.GetKey(), req.Msg.GetValue())
}

func (a *AgonesServer) SetAnnotation(ctx context.Context, req *connect.Request[sdk.KeyValue]) (*connect.Response[sdk.Empty], error) {
	return a.setMetadata(ctx, gatewayv1.SetMetadataRequest_KIND_ANNOTATION, req.Msg.GetKey(), req.Msg.GetValue())
}

func (a *AgonesServer) setMetadata(ctx context.Context, kind gatewayv1.SetMetadataRequest_Kind, key, value string) (*connect.Response[sdk.Empty], error) {
	if key == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("key is required"))
	}
	return a.forward(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_SetMetadata{SetMetadata: &gatewayv1.SetMetadataRequest{
			Kind: kind, Key: key, Value: value,
		}},
	})
}

func (a *AgonesServer) Reserve(ctx context.Context, req *connect.Request[sdk.Duration]) (*connect.Response[sdk.Empty], error) {
	if req.Msg.GetSeconds() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("seconds must not be negative"))
	}
	return a.forward(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Reserve{Reserve: &gatewayv1.ReserveRequest{Seconds: req.Msg.GetSeconds()}},
	})
}

func (a *AgonesServer) forward(ctx context.Context, m *gatewayv1.SidecarMessage) (*connect.Response[sdk.Empty], error) {
	if err := a.sc.send(ctx, m); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&sdk.Empty{}), nil
}

// ToAgonesGameServer maps the arena GameServer onto the Agones message
// shape. ECS-specific information rides on annotations
// (arena.dev/*) so it survives the mapping without extending the vendored
// proto.
func ToAgonesGameServer(gs *arenav1.GameServer) *sdk.GameServer {
	ann := make(map[string]string, len(gs.GetAnnotations())+2)
	for k, v := range gs.GetAnnotations() {
		ann[k] = v
	}
	ann["arena.dev/gameserver-id"] = gs.GetId()
	if gs.GetFleetId() != "" {
		ann["arena.dev/fleet-id"] = gs.GetFleetId()
	}
	out := &sdk.GameServer{
		ObjectMeta: &sdk.GameServer_ObjectMeta{
			Name:              gs.GetName(),
			Namespace:         gs.GetNamespace(),
			Uid:               gs.GetId(),
			Generation:        1,
			CreationTimestamp: gs.GetCreatedAt(),
			Labels:            gs.GetLabels(),
			Annotations:       ann,
		},
		Spec: &sdk.GameServer_Spec{Health: &sdk.GameServer_Spec_Health{}},
		Status: &sdk.GameServer_Status{
			State:   agonesState(gs.GetState()),
			Address: gs.GetAddress(),
		},
	}
	if gs.GetAddress() != "" {
		out.Status.Addresses = []*sdk.GameServer_Status_Address{
			{Type: "ExternalIP", Address: gs.GetAddress()},
		}
	}
	for _, p := range gs.GetPorts() {
		out.Status.Ports = append(out.Status.Ports, &sdk.GameServer_Status_Port{
			Name: p.GetName(),
			Port: p.GetPort(),
		})
	}
	return out
}

// agonesState maps arena states onto Agones GameServerState names. States
// with no Agones equivalent collapse onto the closest lifecycle phase.
func agonesState(s arenav1.GameServer_State) string {
	switch s {
	case arenav1.GameServer_STATE_SCHEDULED, arenav1.GameServer_STATE_STARTING:
		return "Scheduled"
	case arenav1.GameServer_STATE_READY:
		return "Ready"
	case arenav1.GameServer_STATE_ALLOCATED:
		return "Allocated"
	case arenav1.GameServer_STATE_RESERVED:
		return "Reserved"
	case arenav1.GameServer_STATE_DRAINING, arenav1.GameServer_STATE_TERMINATED:
		return "Shutdown"
	case arenav1.GameServer_STATE_UNHEALTHY:
		return "Unhealthy"
	default:
		return ""
	}
}

// reserveSeconds converts an SDK duration to whole seconds, rounding
// sub-second requests up so they never become "reserve forever" (0).
func reserveSeconds(d time.Duration) (int64, error) {
	if d < 0 {
		return 0, fmt.Errorf("duration must not be negative")
	}
	s := int64(d / time.Second)
	if d > 0 && s == 0 {
		s = 1
	}
	return s, nil
}
