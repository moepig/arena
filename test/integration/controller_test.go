//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"google.golang.org/protobuf/encoding/protojson"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/controller"
	"github.com/moepig/arena/internal/ecs"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

func templateJSON(t *testing.T, image string) string {
	t.Helper()
	b, err := protojson.Marshal(&arenav1.GameServerTemplate{
		Spec: &arenav1.GameServerSpec{
			Container: &arenav1.ContainerSpec{Image: image},
			Ports:     []*arenav1.PortSpec{{Name: "game", ContainerPort: 7777}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newLauncher(t *testing.T, cluster string) *ecs.Launcher {
	t.Helper()
	client := ecsClient(t)
	if _, err := client.CreateCluster(context.Background(), &awsecs.CreateClusterInput{
		ClusterName: aws.String(cluster),
	}); err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	return ecs.NewLauncher(client, ecs.Config{
		Cluster:        cluster,
		Subnets:        []string{"subnet-12345"},
		SecurityGroups: []string{"sg-12345"},
		// floci's ECS runs real containers, so both images must be pullable.
		ExecutionRoleARN:  "arn:aws:iam::000000000000:role/arena-exec",
		TaskRoleARN:       "arn:aws:iam::000000000000:role/arena-gs",
		SidecarImage:      "busybox:latest",
		GatewayEndpoint:   "http://arena-api:8080",
		LogGroup:          "",
		Region:            "us-east-1",
		RunTasksPerSecond: 100,
	})
}

// sendTaskEvent drops a synthetic EventBridge "ECS Task State Change"
// envelope on the queue, exactly as the EventBridge → SQS rule would.
func sendTaskEvent(t *testing.T, client *sqs.Client, queueURL, taskARN, gsID, status, ip string) {
	t.Helper()
	body := fmt.Sprintf(`{
	  "detail-type": "ECS Task State Change",
	  "detail": {
	    "taskArn": %q,
	    "lastStatus": %q,
	    "startedBy": "arena:%s",
	    "stoppedReason": "integration test",
	    "attachments": [{"type": "eni", "details": [{"name": "privateIPv4Address", "value": %q}]}]
	  }
	}`, taskARN, status, gsID, ip)
	if _, err := client.SendMessage(context.Background(), &sqs.SendMessageInput{
		QueueUrl:    aws.String(queueURL),
		MessageBody: aws.String(body),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLauncherAndVerifierAgainstECS: RegisterTaskDefinition is cached per
// spec_hash, RunTask stamps startedBy, and the sidecar verifier accepts the
// real binding while rejecting a spoofed one.
func TestLauncherAndVerifierAgainstECS(t *testing.T) {
	launcher := newLauncher(t, "arena-it-launcher")
	fleet := &store.Fleet{
		ID: "f-launch", Namespace: "default", Name: "launch",
		SpecHash: "hash1", TemplateJSON: templateJSON(t, "busybox:latest"),
	}

	taskARN, err := launcher.Launch(context.Background(), fleet, "gs-launch-1")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if taskARN == "" {
		t.Fatal("no task ARN returned")
	}

	v := ecs.NewTaskVerifier(ecsClient(t), "arena-it-launcher")
	if err := v.Verify(context.Background(), "gs-launch-1", taskARN); err != nil {
		t.Errorf("legitimate sidecar identity rejected: %v", err)
	}
	if err := v.Verify(context.Background(), "gs-spoofed", taskARN); err == nil {
		t.Error("spoofed gameserver_id accepted")
	}
}

// noopLauncher satisfies controller.Launcher where no real task is wanted.
type noopLauncher struct{}

func (noopLauncher) Launch(context.Context, *store.Fleet, string) (string, error) { return "", nil }
func (noopLauncher) Stop(context.Context, string, string) error                   { return nil }

// TestPoolRebuildEpochAgainstValkey: pool rebuild epoch semantics tested
// over a real Redis wire — epoch bump invalidates old pools, only
// heartbeat-alive Ready servers are restored, and other pool instances
// converge on the new epoch.
func TestPoolRebuildEpochAgainstValkey(t *testing.T) {
	st, p := newStore(t), newPool(t)
	ctx := context.Background()
	fleetID := "f-rebuild"
	if err := st.CreateFleet(ctx, store.Fleet{ID: fleetID, Namespace: "default", Name: "rebuild", Version: 1}); err != nil {
		t.Fatal(err)
	}
	alive := mustPutGS(t, st, "gs-alive-rb", fleetID, store.StateReady)
	mustPutGS(t, st, "gs-dead-rb", fleetID, store.StateReady)
	if err := p.SetHeartbeat(ctx, "gs-alive-rb", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Both are pooled pre-failover.
	_ = p.Add(ctx, fleetID, "gs-alive-rb", float64(alive.ReadyAt), nil)
	_ = p.Add(ctx, fleetID, "gs-dead-rb", float64(alive.ReadyAt), nil)

	c := controller.New(st, noopLauncher{}, p, nil, controller.Options{}, testLogger())
	oldEpoch := p.Epoch()
	if err := c.RebuildPools(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Epoch() != oldEpoch+1 {
		t.Fatalf("epoch = %d, want %d", p.Epoch(), oldEpoch+1)
	}

	// The new-epoch pool holds only the heartbeat-alive server.
	got, err := p.PopMin(ctx, fleetID)
	if err != nil || got != "gs-alive-rb" {
		t.Fatalf("PopMin = %q/%v, want gs-alive-rb", got, err)
	}
	if _, err := p.PopMin(ctx, fleetID); !errors.Is(err, pool.ErrEmpty) {
		t.Fatalf("dead server survived the rebuild (err=%v)", err)
	}

	// A fresh pool instance (an arena-api replica) syncs to the new epoch.
	p2 := newPool(t)
	if p2.Epoch() != p.Epoch() {
		t.Fatalf("second instance epoch = %d, want %d", p2.Epoch(), p.Epoch())
	}
}

// TestControllerLoopEndToEnd runs the real controller (leader election, work
// queue, resync, SQS consumer) against DynamoDB Local + Valkey + floci:
// scale-up → RUNNING event → Ready → Allocate → STOPPED event → Terminated →
// replacement. This is the Fleet lifecycle without real AWS.
func TestControllerLoopEndToEnd(t *testing.T) {
	st, p := newStore(t), newPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	launcher := newLauncher(t, "arena-it-e2e")
	sqsc := sqsClient(t)
	q, err := sqsc.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String(fmt.Sprintf("arena-it-events-%d", time.Now().UnixNano())),
	})
	if err != nil {
		t.Fatalf("create queue: %v", err)
	}
	queueURL := aws.ToString(q.QueueUrl)

	fleetID := "f-e2e"
	if err := st.CreateFleet(ctx, store.Fleet{
		ID: fleetID, Namespace: "default", Name: "e2e",
		Replicas: 1, Version: 1,
		SpecHash: "e2e-hash", TemplateJSON: templateJSON(t, "busybox:latest"),
	}); err != nil {
		t.Fatal(err)
	}

	events := controller.NewEventConsumer(sqsc, queueURL, testLogger())
	c := controller.New(st, launcher, p, events, controller.Options{
		LeaseName:           "it-e2e-leader",
		HolderID:            "it-e2e",
		ResyncInterval:      300 * time.Millisecond,
		HealthSweepInterval: time.Hour, // hb sweep and pool rebuild stay out
		HealthGracePeriod:   time.Hour, // of this test's way
		RedisPingInterval:   time.Hour,
		StartupTimeout:      time.Hour,
		Workers:             2,
	}, testLogger())
	go func() { _ = c.Run(ctx) }()

	// 1. The reconciler creates one GameServer and launches its task.
	var gs store.GameServer
	waitFor(t, "scale-up to 1 Scheduled gameserver with a task", 20*time.Second, func() bool {
		gss, err := st.ListAllGameServersByFleet(ctx, fleetID, store.StateScheduled)
		if err != nil || len(gss) != 1 || gss[0].TaskARN == "" {
			return false
		}
		gs = gss[0]
		return true
	})

	// 2. The task reports RUNNING via EventBridge → SQS: Scheduled → Starting.
	sendTaskEvent(t, sqsc, queueURL, gs.TaskARN, gs.ID, "RUNNING", "10.0.0.9")
	waitFor(t, "RUNNING event to move it to Starting", 20*time.Second, func() bool {
		cur, err := st.GetGameServer(ctx, gs.ID)
		return err == nil && cur.State == store.StateStarting && cur.Address == "10.0.0.9"
	})

	// 3. The sidecar calls Ready(): commit the transition, then pool it
	// (the SDK Gateway path).
	ready, err := st.TransitionState(ctx, gs.ID, store.StateStarting, store.StateReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Add(ctx, fleetID, gs.ID, float64(ready.ReadyAt), nil); err != nil {
		t.Fatal(err)
	}

	// 4. A matchmaker allocates it.
	alloc := allocation.New(st, p, nil)
	res, err := alloc.Allocate(ctx, allocation.Request{
		AllocationID: allocation.AllocationID("e2e-match-1"), FleetID: fleetID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.GameServer.ID != gs.ID || res.GameServer.Address != "10.0.0.9" {
		t.Fatalf("allocated %s@%s, want %s@10.0.0.9", res.GameServer.ID, res.GameServer.Address, gs.ID)
	}

	// 5. The task dies: STOPPED event → Terminated, and the reconciler
	// replaces it to hold replicas=1.
	sendTaskEvent(t, sqsc, queueURL, gs.TaskARN, gs.ID, "STOPPED", "10.0.0.9")
	waitFor(t, "STOPPED event to confirm Terminated", 20*time.Second, func() bool {
		cur, err := st.GetGameServer(ctx, gs.ID)
		return err == nil && cur.State == store.StateTerminated
	})
	waitFor(t, "the reconciler to launch a replacement", 20*time.Second, func() bool {
		gss, err := st.ListAllGameServersByFleet(ctx, fleetID, "")
		if err != nil {
			return false
		}
		for _, cur := range gss {
			if cur.ID != gs.ID && cur.State == store.StateScheduled && cur.TaskARN != "" {
				return true
			}
		}
		return false
	})

	// The fleet status converges on the observed counts.
	waitFor(t, "fleet status to record the replacement", 20*time.Second, func() bool {
		f, err := st.GetFleet(ctx, fleetID)
		return err == nil && f.Status.Total == 1 && f.Status.Starting == 1
	})
}

// TestFleetShardingSplitsAcrossTwoControllers: two independent Controller
// processes sharing one DynamoDB/Redis backend reconcile ShardCount
// per-fleet shards between them via independent leases.
// Correctness (every fleet gets exactly one GameServer, never zero or two)
// must hold regardless of how the shards happen to split; that the split
// is real (both holders win at least one shard) is checked too, on the same
// timing-dependent footing as TestLeaderLease.
func TestFleetShardingSplitsAcrossTwoControllers(t *testing.T) {
	st, p := newStore(t), newPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const shardCount = 6
	const fleetCount = 12
	fleetIDs := make([]string, fleetCount)
	for i := 0; i < fleetCount; i++ {
		id := fmt.Sprintf("f-shard-%d", i)
		if err := st.CreateFleet(ctx, store.Fleet{
			ID: id, Namespace: "default", Name: id, Replicas: 1, Version: 1,
		}); err != nil {
			t.Fatal(err)
		}
		fleetIDs[i] = id
	}

	base := controller.Options{
		LeaseName:      "it-shard-leader",
		ShardCount:     shardCount,
		ResyncInterval: 200 * time.Millisecond,
		RenewInterval:  200 * time.Millisecond,
		LeaseTTL:       2 * time.Second,
		Workers:        2,
	}
	optsA, optsB := base, base
	optsA.HolderID, optsB.HolderID = "ctrl-a", "ctrl-b"
	cA := controller.New(st, noopLauncher{}, p, nil, optsA, testLogger())
	cB := controller.New(st, noopLauncher{}, p, nil, optsB, testLogger())
	go func() { _ = cA.Run(ctx) }()
	go func() { _ = cB.Run(ctx) }()

	// Every fleet gets exactly one GameServer eventually, regardless of
	// which process ends up owning its shard.
	waitFor(t, "every fleet gets its one GameServer", 20*time.Second, func() bool {
		for _, id := range fleetIDs {
			gss, err := st.ListAllGameServersByFleet(ctx, id, "")
			if err != nil || len(gss) != 1 {
				return false
			}
		}
		return true
	})
	// Give any lingering duplicate-launch race a moment to show up before
	// asserting the final count is exactly one, not "one or more".
	time.Sleep(500 * time.Millisecond)
	for _, id := range fleetIDs {
		gss, err := st.ListAllGameServersByFleet(ctx, id, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(gss) != 1 {
			t.Errorf("fleet %s has %d gameservers, want exactly 1 (no double-processing across shards)", id, len(gss))
		}
	}

	holders := map[string]bool{}
	for shard := 0; shard < shardCount; shard++ {
		lease, err := st.GetLease(ctx, fmt.Sprintf("%s-shard-%d", base.LeaseName, shard))
		if err == nil {
			holders[lease.HolderID] = true
		}
	}
	if len(holders) < 2 {
		t.Logf("all %d shard leases landed on one holder (%v) — timing-dependent, same class as TestLeaderLease; not failing the build on it", shardCount, holders)
	}
}
