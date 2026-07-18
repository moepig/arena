package store

// Record types map 1:1 to the DynamoDB tables. Proto messages are stored
// as protojson strings (Template, Autoscaling) so that
// schema evolution stays in the proto files; attributes the store must
// query or update independently (replicas, state, ...) are top-level.

// Fleet is an item in the `fleets` table. PK: fleet_id.
// GSI namespace-name-index: namespace (PK) + name (SK).
type Fleet struct {
	ID                 string            `dynamodbav:"fleet_id"`
	Namespace          string            `dynamodbav:"namespace"`
	Name               string            `dynamodbav:"name"`
	Labels             map[string]string `dynamodbav:"labels,omitempty"`
	Replicas           int32             `dynamodbav:"replicas"`
	Scheduling         string            `dynamodbav:"scheduling,omitempty"`
	TemplateJSON       string            `dynamodbav:"template,omitempty"`    // protojson arena.v1.GameServerTemplate
	AutoscalingJSON    string            `dynamodbav:"autoscaling,omitempty"` // protojson arena.v1.Autoscaling
	AutoscalingEnabled bool              `dynamodbav:"autoscaling_enabled"`
	StrategyJSON       string            `dynamodbav:"strategy,omitempty"`            // protojson arena.v1.Strategy
	OverflowJSON       string            `dynamodbav:"allocation_overflow,omitempty"` // protojson arena.v1.AllocationOverflow
	CapacityJSON       string            `dynamodbav:"capacity,omitempty"`            // protojson arena.v1.Capacity
	NetworkJSON        string            `dynamodbav:"network,omitempty"`             // protojson arena.v1.Network
	// DrainGraceSeconds is the ECS stopTimeout for game containers;
	// 0 = ECS default.
	DrainGraceSeconds int32       `dynamodbav:"drain_grace_seconds,omitempty"`
	Status            FleetStatus `dynamodbav:"status"`
	Generation        int64       `dynamodbav:"generation"`
	// GenerationAt is when the current generation began (epoch seconds) —
	// the rolling-update drainTimeoutSeconds anchor.
	GenerationAt int64  `dynamodbav:"generation_at,omitempty"`
	SpecHash     string `dynamodbav:"spec_hash"`
	Version      int64  `dynamodbav:"version"`
	CreatedAt    int64  `dynamodbav:"created_at"`
	UpdatedAt    int64  `dynamodbav:"updated_at"`
}

// FleetStatus is the controller-maintained observed state.
type FleetStatus struct {
	Total     int32 `dynamodbav:"total"`
	Ready     int32 `dynamodbav:"ready"`
	Allocated int32 `dynamodbav:"allocated"`
	Starting  int32 `dynamodbav:"starting"`
	Reserved  int32 `dynamodbav:"reserved"`
	// Updated counts active servers on the current spec_hash generation
	// (== Total outside a rolling update).
	Updated int32 `dynamodbav:"updated"`
	// Counters is the fleet-wide Counter aggregation from Redis; autoscaler
	// input and observability, refreshed per reconcile.
	Counters map[string]CounterAggregate `dynamodbav:"counters,omitempty"`
}

// CounterAggregate is a fleet-wide Counter sum.
type CounterAggregate struct {
	Count    int64 `dynamodbav:"count"`
	Capacity int64 `dynamodbav:"capacity"`
}

// Equal reports FleetStatus equality (FleetStatus holds a map, so == is
// unavailable).
func (s FleetStatus) Equal(o FleetStatus) bool {
	if s.Total != o.Total || s.Ready != o.Ready || s.Allocated != o.Allocated ||
		s.Starting != o.Starting || s.Reserved != o.Reserved || s.Updated != o.Updated {
		return false
	}
	if len(s.Counters) != len(o.Counters) {
		return false
	}
	for k, v := range s.Counters {
		if o.Counters[k] != v {
			return false
		}
	}
	return true
}

// GameServer is an item in the `gameservers` table. PK: gameserver_id.
// GSI fleet-index: fleet_id (PK) + state_created (SK, "State#created_at"),
// so begins_with(state_created, "Ready#") lists a fleet's Ready servers.
type GameServer struct {
	ID           string            `dynamodbav:"gameserver_id"`
	FleetID      string            `dynamodbav:"fleet_id"`
	Namespace    string            `dynamodbav:"namespace"`
	Name         string            `dynamodbav:"name"`
	State        State             `dynamodbav:"state"`
	StateCreated string            `dynamodbav:"state_created"` // composite GSI SK, see SortKey
	SpecHash     string            `dynamodbav:"spec_hash"`
	TaskARN      string            `dynamodbav:"task_arn,omitempty"`
	Address      string            `dynamodbav:"ip_address,omitempty"`
	Ports        []Port            `dynamodbav:"ports,omitempty"`
	Labels       map[string]string `dynamodbav:"labels,omitempty"`
	Annotations  map[string]string `dynamodbav:"annotations,omitempty"`
	ReadyAt      int64             `dynamodbav:"ready_at,omitempty"`
	AllocatedAt  int64             `dynamodbav:"allocated_at,omitempty"`
	// ReservedUntil: when a Reserved server auto-returns to Ready (epoch
	// seconds); 0 while Reserved means indefinite.
	ReservedUntil int64 `dynamodbav:"reserved_until,omitempty"`
	Version       int64 `dynamodbav:"version"`
	CreatedAt     int64 `dynamodbav:"created_at"`
	UpdatedAt     int64 `dynamodbav:"updated_at"`
	TTL           int64 `dynamodbav:"ttl,omitempty"` // set on Terminated for auto-expiry
}

// Port is one exposed port of a GameServer.
type Port struct {
	Name     string `dynamodbav:"name"`
	Port     int32  `dynamodbav:"port"`
	Protocol string `dynamodbav:"protocol"` // "UDP" | "TCP"
}

// Allocation is an item in the `allocations` table. PK: allocation_id,
// which is derived from the client idempotency key (UUIDv5) so retries
// converge on the same record.
type Allocation struct {
	ID           string            `dynamodbav:"allocation_id"`
	GameServerID string            `dynamodbav:"gameserver_id"`
	FleetID      string            `dynamodbav:"fleet_id"`
	SessionID    string            `dynamodbav:"session_id,omitempty"`
	Metadata     map[string]string `dynamodbav:"metadata,omitempty"`
	AllocatedAt  int64             `dynamodbav:"allocated_at"`
	ReleasedAt   int64             `dynamodbav:"released_at,omitempty"`
	TTL          int64             `dynamodbav:"ttl,omitempty"`
	// Additional marks a high-density reallocation record: the GameServer
	// stayed Allocated when this was created (AddAllocation,
	// not ClaimGameServer). Releasing it never transitions the GameServer —
	// only the primary (non-Additional) allocation governs that.
	Additional bool `dynamodbav:"additional,omitempty"`
	// ReservedCounters are the Counter names whose aux-ZSET capacity this
	// allocation reserved (1 unit each); Release restores them.
	ReservedCounters []string `dynamodbav:"reserved_counters,omitempty"`
}

// Lease is an item in the `leases` table, used for controller leader
// election.
type Lease struct {
	Name      string `dynamodbav:"lease_name"`
	HolderID  string `dynamodbav:"holder_id"`
	ExpiresAt int64  `dynamodbav:"expires_at"`
}
