package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/store"
)

const (
	defaultNamespace = "default"
	maxReplicas      = 10000
	defaultPageSize  = 100
	maxPageSize      = 1000
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// scaleRetries bounds internal read-modify-write retries when a Scale
// races with controller status updates.
const scaleRetries = 3

// FleetStore is the store surface FleetService needs (*store.Store
// satisfies it; tests substitute an in-memory fake).
type FleetStore interface {
	CreateFleet(ctx context.Context, f store.Fleet) error
	GetFleet(ctx context.Context, fleetID string) (*store.Fleet, error)
	GetFleetByName(ctx context.Context, namespace, name string) (*store.Fleet, error)
	ListFleets(ctx context.Context, namespace string, pageSize int32, pageToken string) ([]store.Fleet, string, error)
	UpdateFleet(ctx context.Context, f store.Fleet) (*store.Fleet, error)
	DeleteFleet(ctx context.Context, fleetID string) error
	ListAllGameServersByFleet(ctx context.Context, fleetID string, state store.State) ([]store.GameServer, error)
}

// FleetServer implements arena.v1.FleetService.
type FleetServer struct {
	arenav1connect.UnimplementedFleetServiceHandler
	store FleetStore
}

func namespaceOrDefault(ns string) string {
	if ns == "" {
		return defaultNamespace
	}
	return ns
}

func validateName(name string) error {
	if name == "" || len(name) > 63 || !namePattern.MatchString(name) {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("invalid name %q: must match %s, max 63 chars", name, namePattern))
	}
	return nil
}

func validateSpec(spec *arenav1.FleetSpec) error {
	if spec == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("spec is required"))
	}
	if r := spec.GetReplicas(); r < 0 || r > maxReplicas {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("replicas %d out of range [0, %d]", r, maxReplicas))
	}
	if err := validateContainers(spec.GetTemplate().GetSpec()); err != nil {
		return err
	}
	for _, p := range spec.GetTemplate().GetSpec().GetPorts() {
		if p.GetName() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("port name is required"))
		}
		switch p.GetPolicy() {
		case arenav1.PortSpec_POLICY_PASSTHROUGH:
			// The server assigns the port at normalization.
			if p.GetContainerPort() != 0 {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("port %q: Passthrough assigns the port; leave container_port unset", p.GetName()))
			}
		default: // Static
			if p.GetContainerPort() < 1 || p.GetContainerPort() > 65535 {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("invalid port %q/%d", p.GetName(), p.GetContainerPort()))
			}
		}
	}
	if err := validateStrategy(spec.GetStrategy(), spec.GetReplicas()); err != nil {
		return err
	}
	if as := spec.GetAutoscaling(); as.GetEnabled() {
		if as.GetMinReplicas() < 0 || as.GetMaxReplicas() < 1 || as.GetMinReplicas() > as.GetMaxReplicas() {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid autoscaling bounds [%d, %d]", as.GetMinReplicas(), as.GetMaxReplicas()))
		}
		if err := validatePolicy(as.GetPolicy(), false); err != nil {
			return err
		}
	}
	for _, p := range spec.GetCapacity().GetProviders() {
		if p.GetName() == "" || p.GetWeight() < 0 || p.GetBase() < 0 {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid capacity provider %q (weight %d, base %d)", p.GetName(), p.GetWeight(), p.GetBase()))
		}
	}
	if g := spec.GetDrainGraceSeconds(); g < 0 || g > 120 {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("drain_grace_seconds %d out of range [0, 120] (Fargate cap)", g))
	}
	return nil
}

// validateContainers checks the single/multi container forms:
// exactly one form, images everywhere, a resolvable game container,
// and mount points referencing declared volumes.
func validateContainers(spec *arenav1.GameServerSpec) error {
	if spec.GetContainer() != nil && len(spec.GetContainers()) > 0 {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("template.spec: container and containers are mutually exclusive"))
	}
	cs := convert.Containers(spec)
	if len(cs) == 0 {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("template.spec.container(.s) is required"))
	}
	volumes := map[string]bool{}
	for _, v := range spec.GetVolumes() {
		if v.GetName() == "" || (v.GetEfs() == nil) == (v.GetHost() == nil) {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("volume %q must set exactly one of efs / host", v.GetName()))
		}
		volumes[v.GetName()] = true
	}
	names := map[string]bool{}
	for _, c := range cs {
		if c.GetImage() == "" {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("container %q: image is required", c.GetName()))
		}
		if len(spec.GetContainers()) > 0 && c.GetName() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("containers entries need a name"))
		}
		if names[c.GetName()] {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("duplicate container name %q", c.GetName()))
		}
		names[c.GetName()] = true
		for _, m := range c.GetMountPoints() {
			if !volumes[m.GetVolume()] {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("container %q mounts undeclared volume %q", c.GetName(), m.GetVolume()))
			}
		}
		for _, s := range c.GetSecrets() {
			if s.GetName() == "" || s.GetValueFrom() == "" {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("container %q: secrets need name and value_from", c.GetName()))
			}
		}
	}
	if len(cs) > 1 && spec.GetGameContainer() == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("game_container is required with multiple containers"))
	}
	if convert.GameContainer(spec) == nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("game_container %q does not match any container", spec.GetGameContainer()))
	}
	return nil
}

// validatePolicy checks an autoscaling policy.
func validatePolicy(p *arenav1.AutoscalingPolicy, nested bool) error {
	switch p.GetType() {
	case arenav1.AutoscalingPolicy_TYPE_WEBHOOK:
		if p.GetWebhook().GetUrl() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("webhook policy needs url"))
		}
	case arenav1.AutoscalingPolicy_TYPE_COUNTER:
		c := p.GetCounter()
		if c.GetKey() == "" {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("counter policy needs key"))
		}
		if c.GetBufferSize() < 0 || c.GetBufferPercent() < 0 || c.GetBufferPercent() > 100 ||
			(c.GetBufferSize() == 0 && c.GetBufferPercent() == 0) {
			return connect.NewError(connect.CodeInvalidArgument,
				errors.New("counter policy needs buffer_size or buffer_percent (0-100)"))
		}
	case arenav1.AutoscalingPolicy_TYPE_CHAIN:
		if nested {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("chain policies cannot nest"))
		}
		if len(p.GetChain()) == 0 {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("chain policy needs entries"))
		}
		for i, e := range p.GetChain() {
			if s := e.GetSchedule(); s != nil && s.GetCron() == "" {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("chain[%d].schedule needs cron", i))
			}
			if err := validatePolicy(e.GetPolicy(), true); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateStrategy checks the rolling-update parameters:
// "25%"-style or absolute counts, and at least one budget non-zero.
func validateStrategy(s *arenav1.Strategy, replicas int32) error {
	if s == nil || s.GetType() == arenav1.Strategy_TYPE_RECREATE {
		return nil
	}
	ru := s.GetRollingUpdate()
	surge, unavailable := int32(1), int32(1) // defaults (25%) are never both zero
	if v := ru.GetMaxSurge(); v != "" {
		n, ok := parsePortion(v, replicas, true)
		if !ok {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid strategy.rollingUpdate.maxSurge %q", v))
		}
		surge = n
	}
	if v := ru.GetMaxUnavailable(); v != "" {
		n, ok := parsePortion(v, replicas, false)
		if !ok {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("invalid strategy.rollingUpdate.maxUnavailable %q", v))
		}
		unavailable = n
	}
	if surge == 0 && unavailable == 0 {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("strategy.rollingUpdate: maxSurge and maxUnavailable cannot both resolve to 0"))
	}
	return nil
}

// parsePortion resolves a "25%"-style or absolute-count string against a
// total (Kubernetes intstr semantics: surge rounds up, unavailable down).
func parsePortion(s string, total int32, roundUp bool) (int32, bool) {
	if p, isPct := strings.CutSuffix(s, "%"); isPct {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		v := float64(total) * float64(n) / 100
		if roundUp {
			return int32(math.Ceil(v)), true
		}
		return int32(math.Floor(v)), true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}

// passthroughPortBase is where server-assigned Passthrough ports start.
// awsvpc gives every task its own ENI, so uniqueness only
// matters within one template.
const passthroughPortBase = 7000

// normalizeSpec rewrites the template ports into their canonical form
// BEFORE SpecHash: Passthrough ports get a deterministic port number, and
// TCPUDP ports expand into a {name}-tcp/{name}-udp pair.
// Deterministic, so the same input spec always hashes the same.
func normalizeSpec(spec *arenav1.FleetSpec) {
	ports := spec.GetTemplate().GetSpec().GetPorts()
	if len(ports) == 0 {
		return
	}
	used := map[int32]bool{}
	for _, p := range ports {
		used[p.GetContainerPort()] = true
	}
	next := int32(passthroughPortBase)
	assign := func() int32 {
		for used[next] {
			next++
		}
		used[next] = true
		return next
	}
	var out []*arenav1.PortSpec
	for _, p := range ports {
		if p.GetPolicy() == arenav1.PortSpec_POLICY_PASSTHROUGH && p.GetContainerPort() == 0 {
			p.ContainerPort = assign()
		}
		if p.GetProtocol() == arenav1.Port_PROTOCOL_TCPUDP {
			tcp := &arenav1.PortSpec{Name: p.GetName() + "-tcp", ContainerPort: p.GetContainerPort(), Protocol: arenav1.Port_PROTOCOL_TCP, Policy: p.GetPolicy()}
			udp := &arenav1.PortSpec{Name: p.GetName() + "-udp", ContainerPort: p.GetContainerPort(), Protocol: arenav1.Port_PROTOCOL_UDP, Policy: p.GetPolicy()}
			out = append(out, tcp, udp)
			continue
		}
		out = append(out, p)
	}
	spec.GetTemplate().GetSpec().Ports = out
}

func (s *FleetServer) CreateFleet(ctx context.Context, req *connect.Request[arenav1.CreateFleetRequest]) (*connect.Response[arenav1.Fleet], error) {
	m := req.Msg
	ns := namespaceOrDefault(m.GetNamespace())
	if err := validateName(m.GetName()); err != nil {
		return nil, err
	}
	if err := validateSpec(m.GetSpec()); err != nil {
		return nil, err
	}
	normalizeSpec(m.GetSpec())

	if _, err := s.store.GetFleetByName(ctx, ns, m.GetName()); err == nil {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("fleet %s/%s already exists", ns, m.GetName()))
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, asConnectError(err)
	}

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
	if err := s.store.CreateFleet(ctx, f); err != nil {
		return nil, asConnectError(err)
	}

	created, err := s.store.GetFleet(ctx, f.ID)
	if err != nil {
		return nil, asConnectError(err)
	}
	out, err := convert.FleetToProto(created)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (s *FleetServer) GetFleet(ctx context.Context, req *connect.Request[arenav1.GetFleetRequest]) (*connect.Response[arenav1.Fleet], error) {
	f, err := s.store.GetFleetByName(ctx, namespaceOrDefault(req.Msg.GetNamespace()), req.Msg.GetName())
	if err != nil {
		return nil, asConnectError(err)
	}
	out, err := convert.FleetToProto(f)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (s *FleetServer) ListFleets(ctx context.Context, req *connect.Request[arenav1.ListFleetsRequest]) (*connect.Response[arenav1.ListFleetsResponse], error) {
	size := req.Msg.GetPageSize()
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	fleets, next, err := s.store.ListFleets(ctx, namespaceOrDefault(req.Msg.GetNamespace()), size, req.Msg.GetPageToken())
	if err != nil {
		return nil, asConnectError(err)
	}
	resp := &arenav1.ListFleetsResponse{NextPageToken: next}
	for i := range fleets {
		f, err := convert.FleetToProto(&fleets[i])
		if err != nil {
			return nil, err
		}
		resp.Fleets = append(resp.Fleets, f)
	}
	return connect.NewResponse(resp), nil
}

func (s *FleetServer) UpdateFleet(ctx context.Context, req *connect.Request[arenav1.UpdateFleetRequest]) (*connect.Response[arenav1.Fleet], error) {
	m := req.Msg
	if m.GetVersion() == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("version is required (optimistic lock)"))
	}
	if err := validateSpec(m.GetSpec()); err != nil {
		return nil, err
	}
	normalizeSpec(m.GetSpec())

	f, err := s.store.GetFleetByName(ctx, namespaceOrDefault(m.GetNamespace()), m.GetName())
	if err != nil {
		return nil, asConnectError(err)
	}
	if f.Version != m.GetVersion() {
		return nil, connect.NewError(connect.CodeAborted,
			fmt.Errorf("version conflict: stored %d, request %d", f.Version, m.GetVersion()))
	}

	// Replicas ownership: while autoscaling is on, the reconciler owns the
	// value; an update that tries to set it is rejected.
	if f.AutoscalingEnabled && m.GetSpec().Replicas != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("replicas is owned by the autoscaler; change autoscaling.min/max instead"))
	}

	hash, err := convert.SpecHash(m.GetSpec().GetTemplate())
	if err != nil {
		return nil, err
	}
	if hash != f.SpecHash {
		f.Generation++
		f.SpecHash = hash
		f.GenerationAt = time.Now().Unix()
	}
	f.Labels = m.GetLabels()
	if err := convert.SpecToStore(m.GetSpec(), f); err != nil {
		return nil, err
	}

	updated, err := s.store.UpdateFleet(ctx, *f)
	if err != nil {
		return nil, asConnectError(err)
	}
	out, err := convert.FleetToProto(updated)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (s *FleetServer) DeleteFleet(ctx context.Context, req *connect.Request[arenav1.DeleteFleetRequest]) (*connect.Response[emptypb.Empty], error) {
	f, err := s.store.GetFleetByName(ctx, namespaceOrDefault(req.Msg.GetNamespace()), req.Msg.GetName())
	if err != nil {
		return nil, asConnectError(err)
	}

	// Refuse while GameServers are still alive; scale to zero first so the
	// controller drains them (no orphaned ECS tasks).
	gss, err := s.store.ListAllGameServersByFleet(ctx, f.ID, "")
	if err != nil {
		return nil, asConnectError(err)
	}
	for _, gs := range gss {
		if gs.State != store.StateTerminated {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("fleet still has non-terminated gameserver %s (%s); scale to 0 first", gs.ID, gs.State))
		}
	}

	if err := s.store.DeleteFleet(ctx, f.ID); err != nil {
		return nil, asConnectError(err)
	}
	return connect.NewResponse(&emptypb.Empty{}), nil
}

func (s *FleetServer) ScaleFleet(ctx context.Context, req *connect.Request[arenav1.ScaleFleetRequest]) (*connect.Response[arenav1.Fleet], error) {
	m := req.Msg
	if m.GetReplicas() < 0 || m.GetReplicas() > maxReplicas {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("replicas %d out of range [0, %d]", m.GetReplicas(), maxReplicas))
	}

	// Version conflicts here come from controller status writes, not user
	// intent, so retry the read-modify-write a few times.
	var lastErr error
	for i := 0; i < scaleRetries; i++ {
		f, err := s.store.GetFleetByName(ctx, namespaceOrDefault(m.GetNamespace()), m.GetName())
		if err != nil {
			return nil, asConnectError(err)
		}
		if f.AutoscalingEnabled {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errors.New("autoscaling is enabled; change autoscaling.min/max instead"))
		}
		f.Replicas = m.GetReplicas()
		updated, err := s.store.UpdateFleet(ctx, *f)
		if errors.Is(err, store.ErrVersionConflict) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, asConnectError(err)
		}
		out, err := convert.FleetToProto(updated)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	}
	return nil, asConnectError(lastErr)
}
