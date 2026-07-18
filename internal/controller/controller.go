// Package controller implements arena-controller: the Fleet reconciler, the
// fleet work queue (per-fleet serialization, cross-fleet parallelism), the
// SQS event consumer for EventBridge ECS task state changes, and
// DynamoDB-lease leader election.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
	"github.com/moepig/arena/internal/telemetry"
)

// Store is the DynamoDB surface the controller needs.
type Store interface {
	GetFleet(ctx context.Context, fleetID string) (*store.Fleet, error)
	ListAllFleets(ctx context.Context) ([]store.Fleet, error)
	UpdateFleet(ctx context.Context, f store.Fleet) (*store.Fleet, error)
	UpdateFleetStatus(ctx context.Context, fleetID string, version int64, st store.FleetStatus) error
	GetGameServer(ctx context.Context, gsID string) (*store.GameServer, error)
	PutGameServer(ctx context.Context, gs store.GameServer) error
	ListAllGameServersByFleet(ctx context.Context, fleetID string, state store.State) ([]store.GameServer, error)
	TransitionState(ctx context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error)
	UpdateGameServerMetadata(ctx context.Context, gsID string, mutate func(*store.GameServer)) (*store.GameServer, error)
	MarkTerminated(ctx context.Context, gsID string) (*store.GameServer, error)
	AcquireLease(ctx context.Context, leaseName, holderID string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, leaseName, holderID string) error
	// PutEvent records an object event; best-effort.
	PutEvent(ctx context.Context, resourceType, resourceID, eventType, reason, message string) error
}

// Launcher starts and stops GameServer tasks (internal/ecs).
type Launcher interface {
	Launch(ctx context.Context, fleet *store.Fleet, gsID string) (taskARN string, err error)
	Stop(ctx context.Context, taskARN, reason string) error
}

// Pool is the Redis surface the controller needs.
type Pool interface {
	Remove(ctx context.Context, fleetID, gsID string) error
	Add(ctx context.Context, fleetID, gsID string, score float64, labels map[string]string) error
	Contains(ctx context.Context, fleetID, gsID string) (bool, error)
	// PublishAllocation pushes a state payload to the server's watch stream
	// (used for allocation-overflow metadata).
	PublishAllocation(ctx context.Context, gsID string, payload []byte) error
	// Heartbeats bulk-checks liveness; result[i] is true when ids[i] has an
	// unexpired heartbeat.
	Heartbeats(ctx context.Context, ids []string) ([]bool, error)
	// Ping / BumpEpoch back the pool rebuilder.
	Ping(ctx context.Context) error
	BumpEpoch(ctx context.Context) (int64, error)
	// Counters bulk-fetches Counter/List snapshots for fleet-status
	// aggregation.
	Counters(ctx context.Context, ids []string) (map[string]pool.Snapshot, error)
}

// InstanceResolver maps an EC2 instance to the arena GameServers running on
// it (EC2 Spot interruption / planned node drain).
type InstanceResolver interface {
	GameServersOnInstance(ctx context.Context, instanceID string) ([]string, error)
}

// AddressResolver turns a task ENI into the address handed to clients
// (public IP). The RUNNING event only carries the ENI id and private IP.
// nil falls back to the private IP.
type AddressResolver interface {
	PublicIP(ctx context.Context, eniID string) (string, error)
}

// Options tune the controller loops.
type Options struct {
	// LeaseName / HolderID identify this instance in leader election.
	LeaseName string
	HolderID  string
	// LeaseTTL / RenewInterval default to 15s / 5s.
	LeaseTTL      time.Duration
	RenewInterval time.Duration
	// ResyncInterval re-enqueues all fleets (level trigger). Default 5m.
	ResyncInterval time.Duration
	// Workers is the reconcile worker-pool size. Default 4.
	Workers int
	// StartupTimeout: Scheduled/Starting older than this go Unhealthy
	// (covers lost RunTask / never-Ready servers). Default 5m.
	StartupTimeout time.Duration
	// HealthSweepInterval re-enqueues all fleets for the heartbeat-expiry
	// sweep. Default 30s.
	HealthSweepInterval time.Duration
	// HealthGracePeriod suppresses heartbeat checks right after a server
	// turns Ready / Allocated. Default 60s.
	HealthGracePeriod time.Duration
	// RedisPingInterval paces the rebuilder's liveness probe. Default 5s.
	RedisPingInterval time.Duration
	// RebuildDelay is how long a recovered Redis gets to collect fresh
	// heartbeats before pools are rebuilt (2 heartbeat cycles). Default 20s.
	RebuildDelay time.Duration
	// MaxLaunchPerReconcile caps one reconcile's scale-up burst. Default 50.
	MaxLaunchPerReconcile int
	// ShardCount splits fleet reconciliation across ShardCount independent
	// leases: FleetShard(fleetID, ShardCount) decides which lease's holder
	// reconciles a given fleet. <= 1 (the default)
	// keeps today's single-leader-does-everything model, using LeaseName
	// unmodified; > 1 additionally runs ShardCount leases named
	// "{LeaseName}-shard-{i}", each independently held and renewed, so
	// multiple controller processes can reconcile in parallel once the
	// fleet count outgrows one leader's reconcile bandwidth.
	ShardCount int
	// AddressResolver resolves task ENIs to public IPs. nil uses the
	// event's private IP.
	AddressResolver AddressResolver
	// Instances resolves EC2 instances to gameservers for Spot interruption
	// drains. nil disables interruption handling (Fargate Spot is covered by
	// the sidecar SIGTERM path either way).
	Instances InstanceResolver
	// Metrics receives the controller metric set (nil = no-op).
	Metrics *telemetry.Metrics
}

func (o *Options) defaults() {
	if o.LeaseName == "" {
		o.LeaseName = "controller-leader"
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = 15 * time.Second
	}
	if o.RenewInterval == 0 {
		o.RenewInterval = 5 * time.Second
	}
	if o.ResyncInterval == 0 {
		o.ResyncInterval = 5 * time.Minute
	}
	if o.Workers == 0 {
		o.Workers = 4
	}
	if o.StartupTimeout == 0 {
		o.StartupTimeout = 5 * time.Minute
	}
	if o.HealthSweepInterval == 0 {
		o.HealthSweepInterval = 30 * time.Second
	}
	if o.HealthGracePeriod == 0 {
		o.HealthGracePeriod = 60 * time.Second
	}
	if o.RedisPingInterval == 0 {
		o.RedisPingInterval = 5 * time.Second
	}
	if o.RebuildDelay == 0 {
		o.RebuildDelay = 20 * time.Second
	}
	if o.MaxLaunchPerReconcile == 0 {
		o.MaxLaunchPerReconcile = 50
	}
	if o.ShardCount <= 0 {
		o.ShardCount = 1
	}
}

// Controller runs the reconcilers under leader election.
type Controller struct {
	store     Store
	launcher  Launcher
	pool      Pool
	events    *EventConsumer // optional; nil disables the SQS consumer
	resolver  AddressResolver
	instances InstanceResolver
	metrics   *telemetry.Metrics
	opts      Options
	log       *slog.Logger

	webhooks *webhookCaller

	// queue backs the unsharded path (ShardCount <= 1) and is also where
	// RebuildPools re-enqueues fleets after a Redis recovery — it exists
	// unconditionally from construction (see New), independent of
	// leadership, so callers can rely on it (e.g. tests exercising
	// handleTaskEvent / RebuildPools directly, without going through Run).
	queue *workQueue
	now   func() time.Time

	// shardMu guards shardQueues: index i is the live queue for shard i
	// while (and only while) this process holds that shard's lease,
	// otherwise nil. Only used when ShardCount > 1.
	shardMu     sync.RWMutex
	shardQueues []*workQueue
}

// New assembles a controller. events may be nil (no SQS consumer).
func New(s Store, l Launcher, p Pool, events *EventConsumer, opts Options, log *slog.Logger) *Controller {
	opts.defaults()
	if log == nil {
		log = slog.Default()
	}
	c := &Controller{
		store:     s,
		launcher:  l,
		pool:      p,
		events:    events,
		resolver:  opts.AddressResolver,
		instances: opts.Instances,
		metrics:   opts.Metrics,
		opts:      opts,
		log:       log,
		webhooks:  newWebhookCaller(),
		queue:     newWorkQueue(),
		now:       time.Now,
	}
	if events != nil {
		events.handler = c.handleTaskEvent
		events.spotHandler = c.handleSpotInterruption
	}
	return c
}

// Run blocks until ctx is done. Unsharded (ShardCount <= 1, the default):
// acquire the single lease, lead until it is lost, repeat — non-leaders are
// hot standbys polling for the lease.
// Sharded (ShardCount > 1): the primary lease (LeaseName) still owns the
// Redis pool rebuilder and the SQS consumer, but fleet reconciliation itself
// runs under ShardCount independent
// "{LeaseName}-shard-{i}" leases, each held/renewed/reconciling
// independently — so this process may lead zero, some, or all shards at
// once, and other processes lead the rest.
func (c *Controller) Run(ctx context.Context) error {
	if c.opts.ShardCount <= 1 {
		return c.runLease(ctx, c.opts.LeaseName, c.lead)
	}

	c.shardQueues = make([]*workQueue, c.opts.ShardCount)
	var wg sync.WaitGroup
	for i := 0; i < c.opts.ShardCount; i++ {
		wg.Add(1)
		shard := i
		leaseName := fmt.Sprintf("%s-shard-%d", c.opts.LeaseName, shard)
		go func() {
			defer wg.Done()
			_ = c.runLease(ctx, leaseName, func(ctx context.Context) { c.leadShard(ctx, shard, leaseName) })
		}()
	}
	err := c.runLease(ctx, c.opts.LeaseName, c.leadPrimary)
	wg.Wait()
	return err
}

// runLease repeats acquire→hold(via leadFn)→lose for one named lease until
// ctx is done. Shared by the unsharded leader loop, the sharded primary
// lease, and every shard lease — only the lease name and what "holding it"
// means (leadFn) differ.
func (c *Controller) runLease(ctx context.Context, leaseName string, leadFn func(context.Context)) error {
	for {
		acquired, err := c.store.AcquireLease(ctx, leaseName, c.opts.HolderID, c.opts.LeaseTTL)
		if err != nil {
			c.log.Warn("lease acquire failed", "lease", leaseName, "error", err)
		}
		if acquired {
			c.log.Info("lease acquired", "lease", leaseName, "holder", c.opts.HolderID)
			leadFn(ctx)
			// Explicit release speeds up standby promotion.
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := c.store.ReleaseLease(releaseCtx, leaseName, c.opts.HolderID); err != nil {
				c.log.Warn("lease release failed", "lease", leaseName, "error", err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(c.opts.RenewInterval):
		}
	}
}

// renewUntilLost renews leaseName on every tick until ctx is done or a
// renewal fails/is denied, calling cancel in the latter case. Shared by
// lead, leadPrimary and leadShard.
func (c *Controller) renewUntilLost(ctx context.Context, leaseName string, cancel context.CancelFunc) {
	ticker := time.NewTicker(c.opts.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := c.store.AcquireLease(ctx, leaseName, c.opts.HolderID, c.opts.LeaseTTL)
			if err != nil || !ok {
				c.log.Warn("lease lost", "lease", leaseName, "holder", c.opts.HolderID, "error", err)
				cancel()
			}
		}
	}
}

// lead runs the full reconcile machinery until ctx is done or the lease is
// lost — the unsharded (ShardCount <= 1) path.
func (c *Controller) lead(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.queue = newWorkQueue() // fresh queue per leadership term
	done := make(chan struct{})
	for i := 0; i < c.opts.Workers; i++ {
		go func() {
			c.worker(ctx, c.queue)
			done <- struct{}{}
		}()
	}
	go c.resyncLoop(ctx, 0, c.queue)
	go c.healthLoop(ctx, 0, c.queue)
	go c.rebuildLoop(ctx)
	if c.events != nil {
		go c.events.Run(ctx)
	}

	c.renewUntilLost(ctx, c.opts.LeaseName, cancel)
	c.queue.ShutDown()
	for i := 0; i < c.opts.Workers; i++ {
		<-done
	}
}

// leadPrimary runs the deployment-wide singleton duties that don't shard —
// the Redis pool rebuilder (one epoch counter for the whole deployment) and
// the SQS consumer (one set of competing consumers is simpler and avoids
// cross-process routing; a fleet event for a shard this process doesn't
// currently hold is dropped by Enqueue and picked up by that shard's own
// resync instead, same as any other missed edge trigger).
func (c *Controller) leadPrimary(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go c.rebuildLoop(ctx)
	if c.events != nil {
		go c.events.Run(ctx)
	}
	c.renewUntilLost(ctx, c.opts.LeaseName, cancel)
}

// leadShard runs one shard's reconcile machinery — its own queue, worker
// pool and resync/health loops, scoped to the fleets FleetShard assigns to
// it — until ctx is done or the shard's lease is lost.
func (c *Controller) leadShard(ctx context.Context, shard int, leaseName string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	q := newWorkQueue()
	c.shardMu.Lock()
	c.shardQueues[shard] = q
	c.shardMu.Unlock()
	defer func() {
		c.shardMu.Lock()
		c.shardQueues[shard] = nil
		c.shardMu.Unlock()
	}()

	done := make(chan struct{})
	for i := 0; i < c.opts.Workers; i++ {
		go func() {
			c.worker(ctx, q)
			done <- struct{}{}
		}()
	}
	go c.resyncLoop(ctx, shard, q)
	go c.healthLoop(ctx, shard, q)

	c.renewUntilLost(ctx, leaseName, cancel)
	q.ShutDown()
	for i := 0; i < c.opts.Workers; i++ {
		<-done
	}
}

// resyncLoop enqueues shard's fleets immediately and on every tick (level
// trigger backing up the edge-triggered events).
func (c *Controller) resyncLoop(ctx context.Context, shard int, q *workQueue) {
	c.resync(ctx, shard, q)
	ticker := time.NewTicker(c.opts.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.resync(ctx, shard, q)
		}
	}
}

// healthLoop re-enqueues shard's fleets on the heartbeat-sweep cadence; the
// sweep itself runs inside reconcileFleet, so health processing for one
// fleet is serialized with its reconcile and autoscale.
func (c *Controller) healthLoop(ctx context.Context, shard int, q *workQueue) {
	ticker := time.NewTicker(c.opts.HealthSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.resync(ctx, shard, q)
		}
	}
}

func (c *Controller) resync(ctx context.Context, shard int, q *workQueue) {
	fleets, err := c.store.ListAllFleets(ctx)
	if err != nil {
		c.log.Warn("resync list failed", "error", err)
		return
	}
	for i := range fleets {
		if FleetShard(fleets[i].ID, c.opts.ShardCount) == shard {
			q.Add(fleets[i].ID)
		}
	}
}

// Enqueue marks a fleet for reconciliation (used by the event consumer).
// Unsharded: today's direct queue add. Sharded: routed to the owning
// shard's queue if this process currently holds that shard's lease,
// otherwise dropped — the shard's actual owner converges via its own
// resync within ResyncInterval regardless, the same safety net an
// unsharded controller relies on for any other missed edge trigger.
func (c *Controller) Enqueue(fleetID string) {
	if c.opts.ShardCount <= 1 {
		c.queue.Add(fleetID)
		return
	}
	shard := FleetShard(fleetID, c.opts.ShardCount)
	c.shardMu.RLock()
	q := c.shardQueues[shard]
	c.shardMu.RUnlock()
	if q != nil {
		q.Add(fleetID)
	}
}

func (c *Controller) worker(ctx context.Context, q *workQueue) {
	for {
		fleetID, ok := q.Get()
		if !ok {
			return
		}
		if err := c.reconcileFleet(ctx, fleetID); err != nil && !errors.Is(err, context.Canceled) {
			c.log.Warn("reconcile failed", "fleet_id", fleetID, "error", err)
			// Errors are retried by the next event or resync tick; there is
			// no per-item backoff queue.
		}
		q.Done(fleetID)
	}
}
