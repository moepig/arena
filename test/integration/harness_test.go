//go:build integration

// Package integration tests arena against a local AWS-equivalent
// environment started by testcontainers: DynamoDB Local (source of truth),
// Valkey (ready pool / heartbeats), and floci (SQS / ECS / STS). Run with:
//
//	make test-integration
//
// These are the concurrency scenarios plus a full controller loop e2e,
// executed against real conditional writes, real transactions, and a
// real Redis wire protocol. (compose.yaml starts the same trio for
// running arena locally; the tests do not use it.)
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/moby/moby/api/types/container"
	"github.com/redis/go-redis/v9"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// Endpoints of the containers TestMain starts.
var (
	dynamoEndpoint string
	redisAddr      string
	flociEndpoint  string // SQS / ECS / STS / EC2
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	env, err := startEnvironment(ctx)
	if env != nil {
		defer env.terminate()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "start local AWS environment:", err)
		return 1
	}
	return m.Run()
}

type environment struct {
	containers []tc.Container
}

func (e *environment) terminate() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, c := range e.containers {
		_ = c.Terminate(ctx)
	}
}

func startEnvironment(ctx context.Context) (*environment, error) {
	env := &environment{}
	start := func(req tc.ContainerRequest) (tc.Container, error) {
		c, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		})
		if c != nil {
			env.containers = append(env.containers, c)
		}
		return c, err
	}
	endpointOf := func(c tc.Container, port string) (string, error) {
		host, err := c.Host(ctx)
		if err != nil {
			return "", err
		}
		mapped, err := c.MappedPort(ctx, port)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s:%s", host, mapped.Port()), nil
	}

	ddb, err := start(tc.ContainerRequest{
		Image:        "amazon/dynamodb-local:2.5.2",
		Cmd:          []string{"-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb"},
		ExposedPorts: []string{"8000/tcp"},
		WaitingFor:   wait.ForListeningPort("8000/tcp"),
	})
	if err != nil {
		return env, fmt.Errorf("dynamodb-local: %w", err)
	}
	addr, err := endpointOf(ddb, "8000/tcp")
	if err != nil {
		return env, err
	}
	dynamoEndpoint = "http://" + addr

	valkey, err := start(tc.ContainerRequest{
		Image:        "valkey/valkey:8.1",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections"),
	})
	if err != nil {
		return env, fmt.Errorf("valkey: %w", err)
	}
	if redisAddr, err = endpointOf(valkey, "6379/tcp"); err != nil {
		return env, err
	}

	// floci's ECS runs real containers, which needs the host docker socket
	// (and root inside the container to read it).
	floci, err := start(tc.ContainerRequest{
		Image:        "floci/floci:latest",
		ExposedPorts: []string{"4566/tcp"},
		WaitingFor:   wait.ForListeningPort("4566/tcp"),
		ConfigModifier: func(c *container.Config) {
			c.User = "root"
		},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, "/var/run/docker.sock:/var/run/docker.sock")
		},
	})
	if err != nil {
		return env, fmt.Errorf("floci: %w", err)
	}
	addr, err = endpointOf(floci, "4566/tcp")
	if err != nil {
		return env, err
	}
	flociEndpoint = "http://" + addr
	return env, nil
}

// awsCfg returns a config with dummy credentials; each client points its
// BaseEndpoint at the local emulator.
func awsCfg(t *testing.T) aws.Config {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"), // floci's default region
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "test")),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func ddbClient(t *testing.T) *dynamodb.Client {
	return dynamodb.NewFromConfig(awsCfg(t), func(o *dynamodb.Options) {
		o.BaseEndpoint = aws.String(dynamoEndpoint)
	})
}

func ecsClient(t *testing.T) *awsecs.Client {
	return awsecs.NewFromConfig(awsCfg(t), func(o *awsecs.Options) {
		o.BaseEndpoint = aws.String(flociEndpoint)
	})
}

func sqsClient(t *testing.T) *sqs.Client {
	return sqs.NewFromConfig(awsCfg(t), func(o *sqs.Options) {
		o.BaseEndpoint = aws.String(flociEndpoint)
	})
}

// newStore creates the four tables under a unique prefix so runs never
// collide, and returns a Store bound to them.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	db := ddbClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prefix := fmt.Sprintf("it%d%04d-", time.Now().UnixMilli(), rand.Intn(10000))
	if err := store.EnsureTables(ctx, db, prefix); err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return store.New(db, store.Options{TablePrefix: prefix})
}

// newPool returns a Pool over the shared Valkey.
func newPool(t *testing.T) *pool.Pool {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { rdb.Close() })
	p := pool.New(rdb)
	if err := p.Sync(context.Background()); err != nil {
		t.Fatalf("valkey unavailable: %v", err)
	}
	return p
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// mustPutGS inserts a GameServer in the given state, walking the state
// machine writes the store enforces.
func mustPutGS(t *testing.T, st *store.Store, gsID, fleetID string, target store.State) *store.GameServer {
	t.Helper()
	ctx := context.Background()
	gs := store.GameServer{ID: gsID, FleetID: fleetID, Namespace: "default", Name: gsID, State: store.StateScheduled}
	if err := st.PutGameServer(ctx, gs); err != nil {
		t.Fatal(err)
	}
	path := map[store.State][]store.State{
		store.StateScheduled: {},
		store.StateStarting:  {store.StateStarting},
		store.StateReady:     {store.StateStarting, store.StateReady},
		store.StateAllocated: {store.StateStarting, store.StateReady, store.StateAllocated},
		store.StateUnhealthy: {store.StateUnhealthy},
	}[target]
	cur := store.StateScheduled
	var out *store.GameServer
	for _, next := range path {
		var err error
		out, err = st.TransitionState(ctx, gsID, cur, next, nil)
		if err != nil {
			t.Fatalf("transition %s -> %s: %v", cur, next, err)
		}
		cur = next
	}
	if out == nil {
		got, err := st.GetGameServer(ctx, gsID)
		if err != nil {
			t.Fatal(err)
		}
		out = got
	}
	return out
}
