// arena-controller runs the reconciler group (Fleet / Health / Autoscale),
// the EventBridge→SQS event consumer, and the pool rebuilder, under
// DynamoDB-lease leader election with a hot standby.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/moepig/arena/internal/controller"
	"github.com/moepig/arena/internal/ecs"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
	"github.com/moepig/arena/internal/telemetry"
)

type config struct {
	redisAddr   string
	tablePrefix string
	queueURL    string
	metricsAddr string
	shardCount  int

	launcher ecs.Config
}

func main() {
	var cfg config
	flag.StringVar(&cfg.redisAddr, "redis", "localhost:6379", "Redis address")
	flag.StringVar(&cfg.tablePrefix, "table-prefix", "arena-", "DynamoDB table name prefix")
	flag.IntVar(&cfg.shardCount, "shard-count", 1, "fleet reconciliation shards: 1 keeps the single-leader model; >1 splits fleets across that many independently-leased shards so multiple controller processes reconcile in parallel")
	flag.StringVar(&cfg.queueURL, "queue-url", "", "SQS queue URL for ECS task state change events (empty disables the consumer)")
	flag.StringVar(&cfg.metricsAddr, "metrics-listen", ":9090", "OpenMetrics /metrics listen address (empty disables)")
	flag.StringVar(&cfg.launcher.Cluster, "cluster", "arena", "ECS cluster for game server tasks")
	subnets := flag.String("subnets", "", "comma-separated subnet IDs for game server tasks")
	securityGroups := flag.String("security-groups", "", "comma-separated security group IDs for game server tasks")
	flag.BoolVar(&cfg.launcher.AssignPublicIP, "assign-public-ip", true, "assign public IPs to game server tasks (clients connect directly)")
	flag.StringVar(&cfg.launcher.ExecutionRoleARN, "execution-role-arn", "", "ECS execution role for game server tasks")
	flag.StringVar(&cfg.launcher.TaskRoleARN, "task-role-arn", "", "task role for game server tasks (CloudWatch Logs only)")
	flag.StringVar(&cfg.launcher.SidecarImage, "sidecar-image", "", "arena-sidecar container image")
	flag.StringVar(&cfg.launcher.GatewayEndpoint, "gateway-endpoint", "", "arena-api SDK Gateway URL handed to sidecars")
	flag.StringVar(&cfg.launcher.LogGroup, "log-group", "", "CloudWatch log group for game server tasks")
	flag.Float64Var(&cfg.launcher.RunTasksPerSecond, "run-tasks-per-second", 5, "RunTask rate limit")
	flag.Parse()
	cfg.launcher.Subnets = splitCSV(*subnets)
	cfg.launcher.SecurityGroups = splitCSV(*securityGroups)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(ctx, log, cfg); err != nil {
		log.Error("arena-controller exited", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, cfg config) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	cfg.launcher.Region = awsCfg.Region
	st := store.New(dynamodb.NewFromConfig(awsCfg), store.Options{TablePrefix: cfg.tablePrefix})

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()
	pl := pool.New(rdb)
	if err := pl.Sync(ctx); err != nil {
		return err
	}
	go pl.WatchEpoch(ctx, time.Minute)

	launcher := ecs.NewLauncher(awsecs.NewFromConfig(awsCfg), cfg.launcher)

	var events *controller.EventConsumer
	if cfg.queueURL != "" {
		events = controller.NewEventConsumer(sqs.NewFromConfig(awsCfg), cfg.queueURL, log)
	} else {
		log.Warn("no -queue-url: running level-triggered only (resync)")
	}

	prom := telemetry.NewPromExporter()
	holder, _ := os.Hostname()
	c := controller.New(st, launcher, pl, events, controller.Options{
		HolderID:        holder + "-" + uuid.NewString()[:8],
		AddressResolver: ecs.NewENIResolver(awsec2.NewFromConfig(awsCfg)),
		Instances:       ecs.NewInstanceResolver(awsecs.NewFromConfig(awsCfg), cfg.launcher.Cluster),
		Metrics:         telemetry.NewMetrics(telemetry.NewEmitter(os.Stdout).WithProm(prom)),
		ShardCount:      cfg.shardCount,
	}, log)

	// OpenMetrics endpoint, served next to nothing else —
	// the controller has no other HTTP surface.
	if cfg.metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", prom)
		srv := &http.Server{Addr: cfg.metricsAddr, Handler: mux}
		go func() {
			<-ctx.Done()
			_ = srv.Close()
		}()
		go func() {
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Warn("metrics server failed", "error", err)
			}
		}()
	}

	log.Info("arena-controller starting", "cluster", cfg.launcher.Cluster)
	return c.Run(ctx)
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
