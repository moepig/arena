package sidecar

// Agones Beta SDK service: the Counters and Lists surface,
// wire-compatible with agones.dev.sdk.beta so official client SDKs work
// unmodified.

import (
	"context"
	"errors"
	"slices"

	"connectrpc.com/connect"

	beta "github.com/moepig/arena/gen/agones/dev/sdk/beta"
	"github.com/moepig/arena/gen/agones/dev/sdk/beta/betaconnect"
)

// AgonesBetaServer implements agones.dev.sdk.beta.SDK over a Sidecar.
type AgonesBetaServer struct {
	betaconnect.UnimplementedSDKHandler
	sc *Sidecar
}

// NewAgonesBetaServer returns the beta (Counters/Lists) service.
func NewAgonesBetaServer(sc *Sidecar) *AgonesBetaServer { return &AgonesBetaServer{sc: sc} }

var _ betaconnect.SDKHandler = (*AgonesBetaServer)(nil)

func betaErr(err error) error {
	switch {
	case errors.Is(err, errNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, errOutOfRange):
		return connect.NewError(connect.CodeOutOfRange, err)
	case errors.Is(err, errAlreadyExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	default:
		return err
	}
}

func (b *AgonesBetaServer) GetCounter(_ context.Context, req *connect.Request[beta.GetCounterRequest]) (*connect.Response[beta.Counter], error) {
	c, err := b.sc.GetCounter(req.Msg.GetName())
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.Counter{Name: req.Msg.GetName(), Count: c.Count, Capacity: c.Capacity}), nil
}

func (b *AgonesBetaServer) UpdateCounter(ctx context.Context, req *connect.Request[beta.UpdateCounterRequest]) (*connect.Response[beta.Counter], error) {
	u := req.Msg.GetCounterUpdateRequest()
	if u.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	c, err := b.sc.UpdateCounter(ctx, u.GetName(), u.Count, u.Capacity, u.GetCountDiff())
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.Counter{Name: u.GetName(), Count: c.Count, Capacity: c.Capacity}), nil
}

func (b *AgonesBetaServer) GetList(_ context.Context, req *connect.Request[beta.GetListRequest]) (*connect.Response[beta.List], error) {
	l, err := b.sc.GetList(req.Msg.GetName())
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.List{Name: req.Msg.GetName(), Capacity: l.Capacity, Values: l.Values}), nil
}

func (b *AgonesBetaServer) UpdateList(ctx context.Context, req *connect.Request[beta.UpdateListRequest]) (*connect.Response[beta.List], error) {
	list := req.Msg.GetList()
	if list.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("list.name is required"))
	}
	paths := req.Msg.GetUpdateMask().GetPaths()
	var capacity *int64
	if slices.Contains(paths, "capacity") {
		v := list.GetCapacity()
		capacity = &v
	}
	setValues := slices.Contains(paths, "values")
	l, err := b.sc.UpdateList(ctx, list.GetName(), capacity, list.GetValues(), setValues)
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.List{Name: list.GetName(), Capacity: l.Capacity, Values: l.Values}), nil
}

func (b *AgonesBetaServer) AddListValue(ctx context.Context, req *connect.Request[beta.AddListValueRequest]) (*connect.Response[beta.List], error) {
	l, err := b.sc.AddListValue(ctx, req.Msg.GetName(), req.Msg.GetValue())
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.List{Name: req.Msg.GetName(), Capacity: l.Capacity, Values: l.Values}), nil
}

func (b *AgonesBetaServer) RemoveListValue(ctx context.Context, req *connect.Request[beta.RemoveListValueRequest]) (*connect.Response[beta.List], error) {
	l, err := b.sc.RemoveListValue(ctx, req.Msg.GetName(), req.Msg.GetValue())
	if err != nil {
		return nil, betaErr(err)
	}
	return connect.NewResponse(&beta.List{Name: req.Msg.GetName(), Capacity: l.Capacity, Values: l.Values}), nil
}
