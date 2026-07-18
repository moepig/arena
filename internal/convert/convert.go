// Package convert maps between the store records (DynamoDB shape) and the
// arena.v1 proto messages. It is shared by the API handlers and the SDK
// Gateway so both express the same wire types.
package convert

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/store"
)

// SpecHash returns the fleet template hash propagated to GameServers
// (rolling-update generation marker). Deterministic marshaling keeps the
// hash stable across processes.
func SpecHash(t *arenav1.GameServerTemplate) (string, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("hash template: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// StateToProto maps a store state to the proto enum.
func StateToProto(s store.State) arenav1.GameServer_State {
	switch s {
	case store.StateScheduled:
		return arenav1.GameServer_STATE_SCHEDULED
	case store.StateStarting:
		return arenav1.GameServer_STATE_STARTING
	case store.StateReady:
		return arenav1.GameServer_STATE_READY
	case store.StateAllocated:
		return arenav1.GameServer_STATE_ALLOCATED
	case store.StateReserved:
		return arenav1.GameServer_STATE_RESERVED
	case store.StateDraining:
		return arenav1.GameServer_STATE_DRAINING
	case store.StateUnhealthy:
		return arenav1.GameServer_STATE_UNHEALTHY
	case store.StateTerminated:
		return arenav1.GameServer_STATE_TERMINATED
	default:
		return arenav1.GameServer_STATE_UNSPECIFIED
	}
}

// StateFromProto maps the proto enum to a store state ("" when unspecified).
func StateFromProto(s arenav1.GameServer_State) store.State {
	switch s {
	case arenav1.GameServer_STATE_SCHEDULED:
		return store.StateScheduled
	case arenav1.GameServer_STATE_STARTING:
		return store.StateStarting
	case arenav1.GameServer_STATE_READY:
		return store.StateReady
	case arenav1.GameServer_STATE_ALLOCATED:
		return store.StateAllocated
	case arenav1.GameServer_STATE_RESERVED:
		return store.StateReserved
	case arenav1.GameServer_STATE_DRAINING:
		return store.StateDraining
	case arenav1.GameServer_STATE_UNHEALTHY:
		return store.StateUnhealthy
	case arenav1.GameServer_STATE_TERMINATED:
		return store.StateTerminated
	default:
		return ""
	}
}

// GameServerToProto converts a store record to the wire message.
func GameServerToProto(gs *store.GameServer) *arenav1.GameServer {
	out := &arenav1.GameServer{
		Id:            gs.ID,
		Name:          gs.Name,
		Namespace:     gs.Namespace,
		FleetId:       gs.FleetID,
		State:         StateToProto(gs.State),
		Address:       gs.Address,
		Labels:        gs.Labels,
		Annotations:   gs.Annotations,
		SpecHash:      gs.SpecHash,
		CreatedAt:     gs.CreatedAt,
		ReadyAt:       gs.ReadyAt,
		AllocatedAt:   gs.AllocatedAt,
		ReservedUntil: gs.ReservedUntil,
		TaskArn:       gs.TaskARN,
	}
	for _, p := range gs.Ports {
		out.Ports = append(out.Ports, &arenav1.Port{
			Name:     p.Name,
			Port:     p.Port,
			Protocol: protocolToProto(p.Protocol),
		})
	}
	return out
}

func protocolToProto(p string) arenav1.Port_Protocol {
	switch p {
	case "TCP":
		return arenav1.Port_PROTOCOL_TCP
	case "UDP":
		return arenav1.Port_PROTOCOL_UDP
	default:
		return arenav1.Port_PROTOCOL_UNSPECIFIED
	}
}

// ProtocolFromProto maps the enum to the store string, defaulting to UDP
// (PortSpec doc: protocol defaults to UDP when unspecified).
func ProtocolFromProto(p arenav1.Port_Protocol) string {
	if p == arenav1.Port_PROTOCOL_TCP {
		return "TCP"
	}
	return "UDP"
}

// FleetToProto converts a store record, rehydrating template/autoscaling
// from their protojson attributes.
func FleetToProto(f *store.Fleet) (*arenav1.Fleet, error) {
	spec, err := SpecFromStore(f)
	if err != nil {
		return nil, err
	}
	status := &arenav1.FleetStatus{
		Total:     f.Status.Total,
		Ready:     f.Status.Ready,
		Allocated: f.Status.Allocated,
		Starting:  f.Status.Starting,
		Reserved:  f.Status.Reserved,
		Updated:   f.Status.Updated,
	}
	for name, c := range f.Status.Counters {
		if status.Counters == nil {
			status.Counters = map[string]*arenav1.CounterAggregate{}
		}
		status.Counters[name] = &arenav1.CounterAggregate{Count: c.Count, Capacity: c.Capacity}
	}
	return &arenav1.Fleet{
		Id:         f.ID,
		Namespace:  f.Namespace,
		Name:       f.Name,
		Labels:     f.Labels,
		Spec:       spec,
		Status:     status,
		Generation: f.Generation,
		SpecHash:   f.SpecHash,
		Version:    f.Version,
		CreatedAt:  f.CreatedAt,
		UpdatedAt:  f.UpdatedAt,
	}, nil
}

// SpecFromStore reassembles the FleetSpec from a store record.
func SpecFromStore(f *store.Fleet) (*arenav1.FleetSpec, error) {
	spec := &arenav1.FleetSpec{
		Replicas:   proto.Int32(f.Replicas),
		Scheduling: schedulingFromString(f.Scheduling),
	}
	if f.TemplateJSON != "" {
		spec.Template = &arenav1.GameServerTemplate{}
		if err := protojson.Unmarshal([]byte(f.TemplateJSON), spec.Template); err != nil {
			return nil, fmt.Errorf("unmarshal template: %w", err)
		}
	}
	if f.AutoscalingJSON != "" {
		spec.Autoscaling = &arenav1.Autoscaling{}
		if err := protojson.Unmarshal([]byte(f.AutoscalingJSON), spec.Autoscaling); err != nil {
			return nil, fmt.Errorf("unmarshal autoscaling: %w", err)
		}
	}
	if f.StrategyJSON != "" {
		spec.Strategy = &arenav1.Strategy{}
		if err := protojson.Unmarshal([]byte(f.StrategyJSON), spec.Strategy); err != nil {
			return nil, fmt.Errorf("unmarshal strategy: %w", err)
		}
	}
	if f.OverflowJSON != "" {
		spec.AllocationOverflow = &arenav1.AllocationOverflow{}
		if err := protojson.Unmarshal([]byte(f.OverflowJSON), spec.AllocationOverflow); err != nil {
			return nil, fmt.Errorf("unmarshal allocation_overflow: %w", err)
		}
	}
	if f.CapacityJSON != "" {
		spec.Capacity = &arenav1.Capacity{}
		if err := protojson.Unmarshal([]byte(f.CapacityJSON), spec.Capacity); err != nil {
			return nil, fmt.Errorf("unmarshal capacity: %w", err)
		}
	}
	if f.NetworkJSON != "" {
		spec.Network = &arenav1.Network{}
		if err := protojson.Unmarshal([]byte(f.NetworkJSON), spec.Network); err != nil {
			return nil, fmt.Errorf("unmarshal network: %w", err)
		}
	}
	spec.DrainGraceSeconds = f.DrainGraceSeconds
	return spec, nil
}

// SpecToStore writes a FleetSpec into the store record fields (template,
// autoscaling, replicas, scheduling). Replicas is only applied when the
// spec carries it (ownership rules).
func SpecToStore(spec *arenav1.FleetSpec, f *store.Fleet) error {
	if spec.Replicas != nil {
		f.Replicas = spec.GetReplicas()
	}
	f.Scheduling = schedulingToString(spec.GetScheduling())
	if spec.GetTemplate() != nil {
		b, err := protojson.Marshal(spec.GetTemplate())
		if err != nil {
			return fmt.Errorf("marshal template: %w", err)
		}
		f.TemplateJSON = string(b)
	} else {
		f.TemplateJSON = ""
	}
	if spec.GetAutoscaling() != nil {
		b, err := protojson.Marshal(spec.GetAutoscaling())
		if err != nil {
			return fmt.Errorf("marshal autoscaling: %w", err)
		}
		f.AutoscalingJSON = string(b)
	} else {
		f.AutoscalingJSON = ""
	}
	f.AutoscalingEnabled = spec.GetAutoscaling().GetEnabled()
	if spec.GetStrategy() != nil {
		b, err := protojson.Marshal(spec.GetStrategy())
		if err != nil {
			return fmt.Errorf("marshal strategy: %w", err)
		}
		f.StrategyJSON = string(b)
	} else {
		f.StrategyJSON = ""
	}
	if spec.GetAllocationOverflow() != nil {
		b, err := protojson.Marshal(spec.GetAllocationOverflow())
		if err != nil {
			return fmt.Errorf("marshal allocation_overflow: %w", err)
		}
		f.OverflowJSON = string(b)
	} else {
		f.OverflowJSON = ""
	}
	if spec.GetCapacity() != nil {
		b, err := protojson.Marshal(spec.GetCapacity())
		if err != nil {
			return fmt.Errorf("marshal capacity: %w", err)
		}
		f.CapacityJSON = string(b)
	} else {
		f.CapacityJSON = ""
	}
	if spec.GetNetwork() != nil {
		b, err := protojson.Marshal(spec.GetNetwork())
		if err != nil {
			return fmt.Errorf("marshal network: %w", err)
		}
		f.NetworkJSON = string(b)
	} else {
		f.NetworkJSON = ""
	}
	f.DrainGraceSeconds = spec.GetDrainGraceSeconds()
	return nil
}

// Containers returns the spec's container list with the single-container
// sugar normalized to a one-element list named "gameserver".
func Containers(spec *arenav1.GameServerSpec) []*arenav1.ContainerSpec {
	if cs := spec.GetContainers(); len(cs) > 0 {
		return cs
	}
	if c := spec.GetContainer(); c != nil {
		single := proto.Clone(c).(*arenav1.ContainerSpec)
		if single.GetName() == "" {
			single.Name = "gameserver"
		}
		return []*arenav1.ContainerSpec{single}
	}
	return nil
}

// GameContainer returns the container that runs the game: the one named by
// game_container, or the only container.
func GameContainer(spec *arenav1.GameServerSpec) *arenav1.ContainerSpec {
	cs := Containers(spec)
	if len(cs) == 0 {
		return nil
	}
	if name := spec.GetGameContainer(); name != "" {
		for _, c := range cs {
			if c.GetName() == name {
				return c
			}
		}
		return nil
	}
	if len(cs) == 1 {
		return cs[0]
	}
	return nil
}

func schedulingToString(s arenav1.FleetSpec_Scheduling) string {
	switch s {
	case arenav1.FleetSpec_SCHEDULING_PACKED:
		return "Packed"
	case arenav1.FleetSpec_SCHEDULING_DISTRIBUTED:
		return "Distributed"
	default:
		return ""
	}
}

func schedulingFromString(s string) arenav1.FleetSpec_Scheduling {
	switch s {
	case "Packed":
		return arenav1.FleetSpec_SCHEDULING_PACKED
	case "Distributed":
		return arenav1.FleetSpec_SCHEDULING_DISTRIBUTED
	default:
		return arenav1.FleetSpec_SCHEDULING_UNSPECIFIED
	}
}

// AllocationToProto converts a store allocation record.
func AllocationToProto(a *store.Allocation) *arenav1.Allocation {
	return &arenav1.Allocation{
		AllocationId: a.ID,
		GameserverId: a.GameServerID,
		FleetId:      a.FleetID,
		SessionId:    a.SessionID,
		Metadata:     a.Metadata,
		AllocatedAt:  a.AllocatedAt,
		ReleasedAt:   a.ReleasedAt,
	}
}

// EncodeStatePush serializes a GameServer for the allocation pub/sub channel
// (protojson of arena.v1.GameServer). The gateway decodes it with
// DecodeStatePush and forwards it on the sidecar stream.
func EncodeStatePush(gs *store.GameServer) []byte {
	b, err := protojson.Marshal(GameServerToProto(gs))
	if err != nil {
		return nil // proto marshal of a well-formed message cannot fail
	}
	return b
}

// DecodeStatePush parses an allocation push payload.
func DecodeStatePush(payload []byte) (*arenav1.GameServer, error) {
	gs := &arenav1.GameServer{}
	if err := protojson.Unmarshal(payload, gs); err != nil {
		return nil, fmt.Errorf("decode state push: %w", err)
	}
	return gs, nil
}
