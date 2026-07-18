// arena-api is the stateless control-plane API server: Fleet CRUD,
// GameServer reads, the Allocation hot path, and the SDK Gateway stream
// endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	"github.com/moepig/arena/internal/allocation"
	"github.com/moepig/arena/internal/api"
	"github.com/moepig/arena/internal/auth"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/ecs"
	"github.com/moepig/arena/internal/gateway"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
	"github.com/moepig/arena/internal/telemetry"
)

func main() {
	addr := flag.String("listen", ":8080", "listen address")
	redisAddr := flag.String("redis", "localhost:6379", "Redis address")
	tablePrefix := flag.String("table-prefix", "arena-", "DynamoDB table name prefix")
	cluster := flag.String("cluster", "", "ECS cluster for sidecar identity verification (empty accepts all — dev only)")
	authzFile := flag.String("authz-file", "", "authorization bindings YAML (empty disables API auth — dev only)")
	serverID := flag.String("server-id", "", "public API hostname tokens must be bound to (required with -authz-file)")
	otlpEndpoint := flag.String("otlp-endpoint", "", "OTLP/gRPC trace collector endpoint, e.g. localhost:4317 (empty disables tracing)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	cfg := apiConfig{
		addr:         *addr,
		redisAddr:    *redisAddr,
		tablePrefix:  *tablePrefix,
		cluster:      *cluster,
		authzFile:    *authzFile,
		serverID:     *serverID,
		otlpEndpoint: *otlpEndpoint,
	}
	if err := run(ctx, log, cfg); err != nil {
		log.Error("arena-api exited", "error", err)
		os.Exit(1)
	}
}

type apiConfig struct {
	addr, redisAddr, tablePrefix, cluster, authzFile, serverID, otlpEndpoint string
}

func run(ctx context.Context, log *slog.Logger, cfg apiConfig) error {
	addr, redisAddr, tablePrefix, cluster := cfg.addr, cfg.redisAddr, cfg.tablePrefix, cfg.cluster
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	st := store.New(dynamodb.NewFromConfig(awsCfg), store.Options{TablePrefix: tablePrefix})

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()
	pl := pool.New(rdb)
	if err := pl.Sync(ctx); err != nil {
		return err
	}
	go pl.WatchEpoch(ctx, time.Minute)

	alloc := allocation.New(st, pl, func(gs *store.GameServer, _ *store.Allocation) []byte {
		return convert.EncodeStatePush(gs)
	})
	prom := telemetry.NewPromExporter()
	alloc.SetMetrics(telemetry.NewMetrics(telemetry.NewEmitter(os.Stdout).WithProm(prom)))

	// Sidecar identity: verify the claimed gameserver_id against the task's
	// startedBy. Without a cluster, accept all (dev).
	var verifier gateway.Verifier
	if cluster != "" {
		verifier = ecs.NewTaskVerifier(awsecs.NewFromConfig(awsCfg), cluster)
	} else {
		log.Warn("no -cluster: sidecar sessions are not verified")
	}

	// Tracing: OTLP → ADOT collector sidecar.
	shutdownTracing, err := telemetry.SetupTracing(ctx, "arena-api", cfg.otlpEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(flushCtx)
	}()

	// API authn/authz: presigned-STS bearer tokens +
	// role-mapping bindings. Absent config disables auth (dev only).
	var handlerOpts []connect.HandlerOption
	if cfg.otlpEndpoint != "" {
		otelInterceptor, err := otelconnect.NewInterceptor(otelconnect.WithTrustRemote())
		if err != nil {
			return err
		}
		handlerOpts = append(handlerOpts, connect.WithInterceptors(otelInterceptor))
	}
	if cfg.authzFile != "" {
		if cfg.serverID == "" {
			return errors.New("-server-id is required with -authz-file (token binding host)")
		}
		src := auth.FileSource(cfg.authzFile)
		data, err := src.Fetch(ctx)
		if err != nil {
			return err
		}
		authzCfg, err := auth.ParseConfig(data)
		if err != nil {
			return err
		}
		authorizer := auth.NewAuthorizer(authzCfg)
		go auth.Watch(ctx, src, time.Minute, authorizer, log)
		handlerOpts = append(handlerOpts, connect.WithInterceptors(
			auth.NewInterceptor(auth.NewSTSVerifier(cfg.serverID), authorizer, log)))
	} else {
		log.Warn("no -authz-file: control-plane API is unauthenticated")
	}

	mux := api.NewMux(st, alloc, handlerOpts...)
	mux.Handle(gatewayv1connect.NewSDKGatewayHandler(gateway.New(st, pl, verifier, log)))
	// OpenMetrics endpoint; EMF stays the primary sink.
	mux.Handle("/metrics", prom)

	// h2c so the sidecar gRPC streams work behind the ALB without local TLS.
	srv := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) // stop accepting, drain in-flight
	}()

	log.Info("arena-api listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
