package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// fakeCtrlStore is an in-memory Store enforcing the same state-machine and
// version conditions as DynamoDB.
type fakeCtrlStore struct {
	fleets      map[string]*store.Fleet
	gameservers map[string]*store.GameServer
	leases      map[string]store.Lease
	statusIn    map[string]store.FleetStatus // last UpdateFleetStatus per fleet
	events      []string                     // "type/id/eventType/reason"
}

func newFakeCtrlStore() *fakeCtrlStore {
	return &fakeCtrlStore{
		fleets:      map[string]*store.Fleet{},
		gameservers: map[string]*store.GameServer{},
		leases:      map[string]store.Lease{},
		statusIn:    map[string]store.FleetStatus{},
	}
}

func (f *fakeCtrlStore) GetFleet(_ context.Context, id string) (*store.Fleet, error) {
	if fl, ok := f.fleets[id]; ok {
		cp := *fl
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCtrlStore) ListAllFleets(_ context.Context) ([]store.Fleet, error) {
	var out []store.Fleet
	for _, fl := range f.fleets {
		out = append(out, *fl)
	}
	return out, nil
}

func (f *fakeCtrlStore) UpdateFleet(_ context.Context, fl store.Fleet) (*store.Fleet, error) {
	cur, ok := f.fleets[fl.ID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if cur.Version != fl.Version {
		return nil, store.ErrVersionConflict
	}
	fl.Version++
	f.fleets[fl.ID] = &fl
	cp := fl
	return &cp, nil
}

func (f *fakeCtrlStore) UpdateFleetStatus(_ context.Context, fleetID string, version int64, st store.FleetStatus) error {
	fl, ok := f.fleets[fleetID]
	if !ok {
		return store.ErrNotFound
	}
	if fl.Version != version {
		return store.ErrVersionConflict
	}
	fl.Status = st
	fl.Version++
	f.statusIn[fleetID] = st
	return nil
}

func (f *fakeCtrlStore) GetGameServer(_ context.Context, id string) (*store.GameServer, error) {
	if gs, ok := f.gameservers[id]; ok {
		cp := *gs
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeCtrlStore) PutGameServer(_ context.Context, gs store.GameServer) error {
	if _, exists := f.gameservers[gs.ID]; exists {
		return store.ErrAlreadyExists
	}
	gs.Version = 1
	f.gameservers[gs.ID] = &gs
	return nil
}

func (f *fakeCtrlStore) ListAllGameServersByFleet(_ context.Context, fleetID string, state store.State) ([]store.GameServer, error) {
	var out []store.GameServer
	for _, gs := range f.gameservers {
		if gs.FleetID == fleetID && (state == "" || gs.State == state) {
			out = append(out, *gs)
		}
	}
	return out, nil
}

func (f *fakeCtrlStore) TransitionState(_ context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if !store.CanTransition(from, to) {
		return nil, fmt.Errorf("%w: illegal %s -> %s", store.ErrConditionFailed, from, to)
	}
	if gs.State != from {
		return nil, fmt.Errorf("%w: state %s, want %s", store.ErrConditionFailed, gs.State, from)
	}
	gs.State = to
	gs.Version++
	if mutate != nil {
		mutate(gs)
	}
	cp := *gs
	return &cp, nil
}

func (f *fakeCtrlStore) UpdateGameServerMetadata(_ context.Context, gsID string, mutate func(*store.GameServer)) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	gs.Version++
	mutate(gs)
	cp := *gs
	return &cp, nil
}

func (f *fakeCtrlStore) MarkTerminated(ctx context.Context, gsID string) (*store.GameServer, error) {
	gs, ok := f.gameservers[gsID]
	if !ok {
		return nil, store.ErrNotFound
	}
	if gs.State == store.StateTerminated {
		cp := *gs
		return &cp, nil
	}
	return f.TransitionState(ctx, gsID, gs.State, store.StateTerminated, nil)
}

func (f *fakeCtrlStore) PutEvent(_ context.Context, resourceType, resourceID, eventType, reason, _ string) error {
	f.events = append(f.events, resourceType+"/"+resourceID+"/"+eventType+"/"+reason)
	return nil
}

func (f *fakeCtrlStore) AcquireLease(_ context.Context, name, holder string, _ time.Duration) (bool, error) {
	l, ok := f.leases[name]
	if ok && l.HolderID != holder {
		return false, nil
	}
	f.leases[name] = store.Lease{Name: name, HolderID: holder}
	return true, nil
}

func (f *fakeCtrlStore) ReleaseLease(_ context.Context, name, holder string) error {
	if l, ok := f.leases[name]; ok && l.HolderID == holder {
		delete(f.leases, name)
	}
	return nil
}

// fakeLauncher records launches/stops; err makes Launch fail. Guarded by mu
// so shard tests may poll launchedCount() from a different goroutine while
// a background reconcile is still in flight.
type fakeLauncher struct {
	mu       sync.Mutex
	launched []string
	stopped  []string
	taskARN  func(gsID string) string
	err      error
}

func (f *fakeLauncher) Launch(_ context.Context, _ *store.Fleet, gsID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.mu.Lock()
	f.launched = append(f.launched, gsID)
	f.mu.Unlock()
	if f.taskARN != nil {
		return f.taskARN(gsID), nil
	}
	return "arn:task/" + gsID, nil
}

// launchedCount safely reads len(launched); use this instead of len(f.launched)
// when a background reconcile goroutine may still be running concurrently.
func (f *fakeLauncher) launchedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launched)
}

func (f *fakeLauncher) Stop(_ context.Context, taskARN, _ string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, taskARN)
	f.mu.Unlock()
	return nil
}

type fakeCtrlPool struct {
	removed []string        // "fleetID/gsID"
	added   []string        // "fleetID/gsID"
	pooled  map[string]bool // "fleetID/gsID" membership
	// heartbeats: gsID → alive. Missing key defaults to alive so tests not
	// about health don't have to seed it.
	heartbeats map[string]bool
	hbErr      error
	published  []string // gsIDs pushed via PublishAllocation
	counters   map[string]pool.Snapshot

	epoch   atomic.Int64
	pingErr atomic.Pointer[error]
}

func (f *fakeCtrlPool) Ping(context.Context) error {
	if p := f.pingErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (f *fakeCtrlPool) setPingErr(err error) { f.pingErr.Store(&err) }

func (f *fakeCtrlPool) BumpEpoch(context.Context) (int64, error) {
	return f.epoch.Add(1), nil
}

func (f *fakeCtrlPool) Remove(_ context.Context, fleetID, gsID string) error {
	f.removed = append(f.removed, fleetID+"/"+gsID)
	delete(f.pooled, fleetID+"/"+gsID)
	return nil
}

func (f *fakeCtrlPool) Add(_ context.Context, fleetID, gsID string, _ float64, _ map[string]string) error {
	f.added = append(f.added, fleetID+"/"+gsID)
	if f.pooled == nil {
		f.pooled = map[string]bool{}
	}
	f.pooled[fleetID+"/"+gsID] = true
	return nil
}

func (f *fakeCtrlPool) Contains(_ context.Context, fleetID, gsID string) (bool, error) {
	return f.pooled[fleetID+"/"+gsID], nil
}

func (f *fakeCtrlPool) PublishAllocation(_ context.Context, gsID string, _ []byte) error {
	f.published = append(f.published, gsID)
	return nil
}

func (f *fakeCtrlPool) Heartbeats(_ context.Context, ids []string) ([]bool, error) {
	if f.hbErr != nil {
		return nil, f.hbErr
	}
	out := make([]bool, len(ids))
	for i, id := range ids {
		alive, seeded := f.heartbeats[id]
		out[i] = !seeded || alive
	}
	return out, nil
}

func (f *fakeCtrlPool) Counters(_ context.Context, ids []string) (map[string]pool.Snapshot, error) {
	out := make(map[string]pool.Snapshot, len(ids))
	for _, id := range ids {
		if snap, ok := f.counters[id]; ok {
			out[id] = snap
		}
	}
	return out, nil
}

func newTestController(fs *fakeCtrlStore, fl *fakeLauncher, fp *fakeCtrlPool) *Controller {
	c := New(fs, fl, fp, nil, Options{}, slog.Default())
	c.now = func() time.Time { return time.Unix(10_000, 0) }
	return c
}

func addFleet(fs *fakeCtrlStore, id string, replicas int32) *store.Fleet {
	fl := &store.Fleet{ID: id, Namespace: "default", Name: id, Replicas: replicas, Version: 1}
	fs.fleets[id] = fl
	return fl
}

func addGS(fs *fakeCtrlStore, id, fleetID string, state store.State, createdAt int64) *store.GameServer {
	gs := &store.GameServer{
		ID: id, FleetID: fleetID, State: state, Version: 1,
		CreatedAt: createdAt, TaskARN: "arn:task/" + id,
	}
	switch state {
	case store.StateReady:
		gs.ReadyAt = createdAt
	case store.StateAllocated:
		gs.ReadyAt = createdAt
		gs.AllocatedAt = createdAt
	}
	fs.gameservers[id] = gs
	return gs
}

func TestReconcileScaleUp(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 3)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fl.launched) != 3 {
		t.Fatalf("launched %d tasks, want 3", len(fl.launched))
	}
	scheduled := 0
	for _, gs := range fs.gameservers {
		if gs.State != store.StateScheduled {
			t.Errorf("gs %s state = %s, want Scheduled", gs.ID, gs.State)
		}
		if gs.TaskARN == "" {
			t.Errorf("gs %s missing task ARN write-back", gs.ID)
		}
		scheduled++
	}
	if scheduled != 3 {
		t.Fatalf("created %d gameservers, want 3", scheduled)
	}
}

func TestReconcileScaleUpCapped(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 100)
	c := New(fs, fl, fp, nil, Options{MaxLaunchPerReconcile: 10}, slog.Default())

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fl.launched) != 10 {
		t.Fatalf("launched %d tasks, want cap of 10", len(fl.launched))
	}
}

func TestReconcileScaleDownDrainsOldestReady(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-old", "f1", store.StateReady, 100)
	addGS(fs, "gs-mid", "f1", store.StateReady, 200)
	addGS(fs, "gs-new", "f1", store.StateReady, 300)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-old"].State; got != store.StateDraining {
		t.Errorf("gs-old state = %s, want Draining", got)
	}
	if got := fs.gameservers["gs-mid"].State; got != store.StateDraining {
		t.Errorf("gs-mid state = %s, want Draining", got)
	}
	if got := fs.gameservers["gs-new"].State; got != store.StateReady {
		t.Errorf("gs-new state = %s, want Ready (survivor)", got)
	}
	if len(fp.removed) != 2 {
		t.Errorf("pool removals = %v, want 2", fp.removed)
	}
	if len(fl.stopped) != 2 {
		t.Errorf("stopped tasks = %v, want 2", fl.stopped)
	}
}

func TestReconcileScaleDownNeverTouchesAllocated(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 0)
	addGS(fs, "gs-a", "f1", store.StateAllocated, 100)
	addGS(fs, "gs-r", "f1", store.StateReady, 200)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-a"].State; got != store.StateAllocated {
		t.Errorf("allocated server state = %s, want Allocated", got)
	}
	if got := fs.gameservers["gs-r"].State; got != store.StateDraining {
		t.Errorf("ready server state = %s, want Draining", got)
	}
}

func TestReconcileStartupTimeout(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	// now = 10_000; default StartupTimeout is 5m, so created at 9_000 is late.
	addGS(fs, "gs-stuck", "f1", store.StateStarting, 9_000)
	fresh := addGS(fs, "gs-fresh", "f1", store.StateStarting, 9_990)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-stuck"].State; got != store.StateUnhealthy {
		t.Errorf("stuck server state = %s, want Unhealthy", got)
	}
	if fresh.State != store.StateStarting {
		t.Errorf("fresh server state = %s, want Starting", fresh.State)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != "arn:task/gs-stuck" {
		t.Errorf("stopped = %v, want [arn:task/gs-stuck]", fl.stopped)
	}
	// The failed server no longer counts as active → one replacement.
	if len(fl.launched) != 1 {
		t.Errorf("launched = %v, want 1 replacement", fl.launched)
	}
}

func TestReconcileStartupTimeoutWithoutTask(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 0)
	gs := addGS(fs, "gs-lost", "f1", store.StateScheduled, 1_000)
	gs.TaskARN = "" // RunTask never succeeded: no STOPPED event will come
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-lost"].State; got != store.StateTerminated {
		t.Errorf("state = %s, want Terminated (confirmed directly)", got)
	}
	if len(fl.stopped) != 0 {
		t.Errorf("stopped = %v, want none", fl.stopped)
	}
}

func TestReconcileStopsUnhealthyAndDraining(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 0)
	addGS(fs, "gs-u", "f1", store.StateUnhealthy, 100)
	addGS(fs, "gs-d", "f1", store.StateDraining, 100)
	addGS(fs, "gs-t", "f1", store.StateTerminated, 100)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 2 {
		t.Errorf("stopped = %v, want gs-u and gs-d tasks", fl.stopped)
	}
	if len(fl.launched) != 0 {
		t.Errorf("launched = %v, want none (replicas 0)", fl.launched)
	}
}

func TestReconcileUpdatesFleetStatus(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 3)
	addGS(fs, "gs-1", "f1", store.StateReady, 100)
	addGS(fs, "gs-2", "f1", store.StateAllocated, 100)
	addGS(fs, "gs-3", "f1", store.StateStarting, 9_990)
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	want := store.FleetStatus{Total: 3, Ready: 1, Allocated: 1, Starting: 1, Updated: 3}
	if got := fs.statusIn["f1"]; !got.Equal(want) {
		t.Errorf("status = %+v, want %+v", got, want)
	}
}

func TestReconcileDeletedFleetIsNoop(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	c := newTestController(fs, fl, fp)
	if err := c.reconcileFleet(context.Background(), "gone"); err != nil {
		t.Fatalf("deleted fleet should be a no-op, got %v", err)
	}
}

func TestHealthSweepFailsExpiredHeartbeat(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-dead", "f1", store.StateReady, 1_000) // Ready long past grace
	fp.heartbeats = map[string]bool{"gs-dead": false}
	fp.pooled = map[string]bool{"f1/gs-dead": true}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-dead"].State; got != store.StateUnhealthy {
		t.Errorf("state = %s, want Unhealthy", got)
	}
	if len(fp.removed) != 1 || fp.removed[0] != "f1/gs-dead" {
		t.Errorf("pool removals = %v, want [f1/gs-dead]", fp.removed)
	}
	if len(fl.stopped) != 1 {
		t.Errorf("stopped = %v, want the dead server's task", fl.stopped)
	}
	// The dead server no longer counts as active → one replacement.
	if len(fl.launched) != 1 {
		t.Errorf("launched = %v, want 1 replacement", fl.launched)
	}
}

func TestHealthSweepHonorsGracePeriod(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	// now = 10_000, default grace 60s.
	addGS(fs, "gs-fresh", "f1", store.StateReady, 9_990) // Ready 10s ago
	alloc := addGS(fs, "gs-alloc", "f1", store.StateAllocated, 1_000)
	alloc.AllocatedAt = 9_990 // allocated 10s ago, Ready long before
	fp.heartbeats = map[string]bool{"gs-fresh": false, "gs-alloc": false}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-fresh"].State; got != store.StateReady {
		t.Errorf("fresh Ready state = %s, want Ready (grace)", got)
	}
	if got := fs.gameservers["gs-alloc"].State; got != store.StateAllocated {
		t.Errorf("fresh Allocated state = %s, want Allocated (grace anchored at allocated_at)", got)
	}
}

func TestHealthSweepSkipsOnRedisError(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateReady, 1_000)
	fp.heartbeats = map[string]bool{"gs-1": false}
	fp.hbErr = context.DeadlineExceeded // Redis unreachable
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateReady {
		t.Errorf("state = %s, want Ready (never write off on a monitoring outage)", got)
	}
}

func TestHealthSweepRepairsMissingPoolEntry(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 2)
	addGS(fs, "gs-lost", "f1", store.StateReady, 1_000)   // healthy, not pooled
	addGS(fs, "gs-pooled", "f1", store.StateReady, 1_000) // healthy, pooled
	fp.pooled = map[string]bool{"f1/gs-pooled": true}
	c := newTestController(fs, fl, fp)

	if err := c.reconcileFleet(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	if len(fp.added) != 1 || fp.added[0] != "f1/gs-lost" {
		t.Errorf("pool adds = %v, want [f1/gs-lost]", fp.added)
	}
}

func taskEvent(gsID, status string) TaskStateChange {
	return TaskStateChange{
		TaskARN:    "arn:task/" + gsID,
		LastStatus: status,
		StartedBy:  "arena:" + gsID,
		ENIID:      "eni-1",
		PrivateIP:  "10.0.0.5",
	}
}

func TestHandleTaskEventRunning(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	gs := addGS(fs, "gs-1", "f1", store.StateScheduled, 9_990)
	gs.TaskARN = ""
	c := newTestController(fs, fl, fp)

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-1", "RUNNING")); err != nil {
		t.Fatal(err)
	}
	got := fs.gameservers["gs-1"]
	if got.State != store.StateStarting {
		t.Errorf("state = %s, want Starting", got.State)
	}
	if got.Address != "10.0.0.5" {
		t.Errorf("address = %q, want private IP fallback", got.Address)
	}
	if got.TaskARN != "arn:task/gs-1" {
		t.Errorf("task arn = %q", got.TaskARN)
	}
	if id, ok := c.queue.Get(); !ok || id != "f1" {
		t.Errorf("fleet not enqueued after RUNNING (got %q, %v)", id, ok)
	}
}

// resolverFunc adapts a func to AddressResolver.
type resolverFunc func(ctx context.Context, eniID string) (string, error)

func (f resolverFunc) PublicIP(ctx context.Context, eniID string) (string, error) {
	return f(ctx, eniID)
}

func TestHandleTaskEventRunningResolvesPublicIP(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateScheduled, 9_990)
	c := New(fs, fl, fp, nil, Options{
		AddressResolver: resolverFunc(func(_ context.Context, eniID string) (string, error) {
			if eniID != "eni-1" {
				t.Errorf("resolver got eni %q", eniID)
			}
			return "203.0.113.24", nil
		}),
	}, slog.Default())

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-1", "RUNNING")); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].Address; got != "203.0.113.24" {
		t.Errorf("address = %q, want resolved public IP", got)
	}
}

func TestHandleTaskEventRunningDuplicateIsAbsorbed(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateReady, 9_990) // already progressed
	c := newTestController(fs, fl, fp)

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-1", "RUNNING")); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 0 {
		t.Errorf("duplicate RUNNING stopped a live task: %v", fl.stopped)
	}
}

func TestHandleTaskEventRunningStopsDefunct(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateTerminated, 9_990)
	c := newTestController(fs, fl, fp)

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-1", "RUNNING")); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 1 {
		t.Errorf("task for written-off server not stopped: %v", fl.stopped)
	}
}

func TestHandleTaskEventRunningOrphan(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	c := newTestController(fs, fl, fp)

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-nope", "RUNNING")); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 1 || fl.stopped[0] != "arn:task/gs-nope" {
		t.Errorf("orphan task not stopped: %v", fl.stopped)
	}
}

func TestHandleTaskEventStopped(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	addFleet(fs, "f1", 1)
	addGS(fs, "gs-1", "f1", store.StateReady, 9_990)
	c := newTestController(fs, fl, fp)

	if err := c.handleTaskEvent(context.Background(), taskEvent("gs-1", "STOPPED")); err != nil {
		t.Fatal(err)
	}
	if got := fs.gameservers["gs-1"].State; got != store.StateTerminated {
		t.Errorf("state = %s, want Terminated", got)
	}
	if len(fp.removed) != 1 || fp.removed[0] != "f1/gs-1" {
		t.Errorf("pool removal = %v, want [f1/gs-1]", fp.removed)
	}
	if id, ok := c.queue.Get(); !ok || id != "f1" {
		t.Errorf("fleet not enqueued for replenishment (got %q, %v)", id, ok)
	}
}

func TestHandleTaskEventIgnoresForeignTasks(t *testing.T) {
	fs, fl, fp := newFakeCtrlStore(), &fakeLauncher{}, &fakeCtrlPool{}
	c := newTestController(fs, fl, fp)
	ev := taskEvent("gs-1", "RUNNING")
	ev.StartedBy = "ecs-svc/deploy-123"
	if err := c.handleTaskEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(fl.stopped) != 0 {
		t.Errorf("foreign task touched: %v", fl.stopped)
	}
}

func TestParseTaskEvent(t *testing.T) {
	body := `{
	  "detail-type": "ECS Task State Change",
	  "detail": {
	    "taskArn": "arn:aws:ecs:ap-northeast-1:123:task/arena/abc",
	    "lastStatus": "RUNNING",
	    "startedBy": "arena:gs-1",
	    "attachments": [{
	      "type": "eni",
	      "details": [
	        {"name": "networkInterfaceId", "value": "eni-0abc"},
	        {"name": "privateIPv4Address", "value": "10.0.1.7"}
	      ]
	    }]
	  }
	}`
	ev, err := parseQueueEvent(body)
	if err != nil {
		t.Fatal(err)
	}
	want := TaskStateChange{
		TaskARN:    "arn:aws:ecs:ap-northeast-1:123:task/arena/abc",
		LastStatus: "RUNNING",
		StartedBy:  "arena:gs-1",
		ENIID:      "eni-0abc",
		PrivateIP:  "10.0.1.7",
	}
	if ev.task == nil || *ev.task != want {
		t.Errorf("parsed = %+v, want %+v", ev.task, want)
	}

	if ev, err := parseQueueEvent(`{"detail-type": "ECS Container Instance State Change"}`); err != nil || ev != nil {
		t.Errorf("ignored detail-type: ev=%v err=%v, want nil/nil", ev, err)
	}
	if _, err := parseQueueEvent("not json"); err == nil {
		t.Error("malformed body should error")
	}

	// EC2 Spot interruption warnings resolve to the instance ID.
	spot, err := parseQueueEvent(`{"detail-type": "EC2 Spot Instance Interruption Warning", "detail": {"instance-id": "i-0abc"}}`)
	if err != nil || spot == nil || spot.spotInstanceID != "i-0abc" {
		t.Errorf("spot warning parsed = %+v err=%v, want instance i-0abc", spot, err)
	}
}
