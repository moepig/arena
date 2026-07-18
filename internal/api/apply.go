package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"google.golang.org/protobuf/testing/protocmp"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/store"
)

// applyRetries bounds the internal read-modify-write loop: version conflicts
// here come from controller status/autoscale writes, not user intent, so the
// server retries instead of surfacing them.
const applyRetries = 3

// ApplyFleet is the declarative upsert behind `arenactl apply`.
// Identity is (namespace, name); the spec is replaced wholesale
// except for the replicas ownership rules.
func (s *FleetServer) ApplyFleet(ctx context.Context, req *connect.Request[arenav1.ApplyFleetRequest]) (*connect.Response[arenav1.ApplyFleetResponse], error) {
	m := req.Msg
	ns := namespaceOrDefault(m.GetNamespace())
	if err := validateName(m.GetName()); err != nil {
		return nil, err
	}
	if err := validateSpec(m.GetSpec()); err != nil {
		return nil, err
	}
	normalizeSpec(m.GetSpec())
	// The HPA + `kubectl apply` scale-rollback accident, prevented by type:
	// an autoscaled fleet's manifest must not pin replicas.
	if m.GetSpec().GetAutoscaling().GetEnabled() && m.GetSpec().Replicas != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("autoscaling is enabled: remove spec.replicas from the manifest (the autoscaler owns it)"))
	}

	var lastErr error
	for attempt := 0; attempt < applyRetries; attempt++ {
		cur, err := s.store.GetFleetByName(ctx, ns, m.GetName())
		if errors.Is(err, store.ErrNotFound) {
			resp, err := s.applyCreate(ctx, ns, m)
			if errors.Is(err, store.ErrAlreadyExists) {
				lastErr = err // racing create; retry as update
				continue
			}
			if err != nil {
				return nil, asConnectError(err)
			}
			return resp, nil
		}
		if err != nil {
			return nil, asConnectError(err)
		}

		resp, err := s.applyUpdate(ctx, cur, m)
		if errors.Is(err, store.ErrVersionConflict) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, asConnectError(err)
		}
		return resp, nil
	}
	return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("apply contention, retry: %w", lastErr))
}

func (s *FleetServer) applyCreate(ctx context.Context, ns string, m *arenav1.ApplyFleetRequest) (*connect.Response[arenav1.ApplyFleetResponse], error) {
	hash, err := convert.SpecHash(m.GetSpec().GetTemplate())
	if err != nil {
		return nil, err
	}
	f := store.Fleet{
		ID:         uuid.NewString(),
		Namespace:  ns,
		Name:       m.GetName(),
		Labels:     m.GetLabels(),
		Generation: 1,
		SpecHash:   hash,
		Version:    1,
	}
	if err := convert.SpecToStore(m.GetSpec(), &f); err != nil {
		return nil, err
	}
	if m.GetSpec().Replicas == nil && f.AutoscalingEnabled {
		// Start at the floor; the Autoscale reconciler owns it from here.
		f.Replicas = m.GetSpec().GetAutoscaling().GetMinReplicas()
	}

	normalized, err := convert.SpecFromStore(&f)
	if err != nil {
		return nil, err
	}
	diff := cmp.Diff(&arenav1.FleetSpec{}, normalized, protocmp.Transform())

	if !m.GetDryRun() {
		if err := s.store.CreateFleet(ctx, f); err != nil {
			return nil, err
		}
		created, err := s.store.GetFleet(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		f = *created
	}
	out, err := convert.FleetToProto(&f)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&arenav1.ApplyFleetResponse{
		Action:         arenav1.ApplyFleetResponse_ACTION_CREATED,
		Fleet:          out,
		NormalizedSpec: normalized,
		Diff:           diff,
	}), nil
}

func (s *FleetServer) applyUpdate(ctx context.Context, cur *store.Fleet, m *arenav1.ApplyFleetRequest) (*connect.Response[arenav1.ApplyFleetResponse], error) {
	desired := *cur
	desired.Labels = m.GetLabels()
	// SpecToStore keeps the current replicas when the manifest omits it —
	// which the ownership check has already forced for autoscaled fleets.
	if err := convert.SpecToStore(m.GetSpec(), &desired); err != nil {
		return nil, err
	}
	hash, err := convert.SpecHash(m.GetSpec().GetTemplate())
	if err != nil {
		return nil, err
	}
	if hash != cur.SpecHash {
		desired.Generation++
		desired.SpecHash = hash
		desired.GenerationAt = time.Now().Unix()
	}

	curSpec, err := convert.SpecFromStore(cur)
	if err != nil {
		return nil, err
	}
	desiredSpec, err := convert.SpecFromStore(&desired)
	if err != nil {
		return nil, err
	}
	diff := cmp.Diff(curSpec, desiredSpec, protocmp.Transform())
	if labelsDiff := cmp.Diff(cur.Labels, desired.Labels); labelsDiff != "" {
		diff += labelsDiff
	}

	if diff == "" {
		out, err := convert.FleetToProto(cur)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&arenav1.ApplyFleetResponse{
			Action:         arenav1.ApplyFleetResponse_ACTION_UNCHANGED,
			Fleet:          out,
			NormalizedSpec: desiredSpec,
		}), nil
	}

	result := &desired
	if !m.GetDryRun() {
		if result, err = s.store.UpdateFleet(ctx, desired); err != nil {
			return nil, err
		}
	}
	out, err := convert.FleetToProto(result)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&arenav1.ApplyFleetResponse{
		Action:         arenav1.ApplyFleetResponse_ACTION_UPDATED,
		Fleet:          out,
		NormalizedSpec: desiredSpec,
		Diff:           diff,
	}), nil
}
