// arena-sidecar serves the Agones-compatible SDK (arena/v1/sdk.proto) to the
// game server container on localhost:9357 and multiplexes everything over a
// single outbound SDKGateway stream to arena-api. It has no access to
// DynamoDB, Redis, or the ECS API.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/moepig/arena/gen/agones/dev/sdk/beta/betaconnect"
	"github.com/moepig/arena/gen/agones/dev/sdk/sdkconnect"
	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/sidecar"
)

func main() {
	// AGONES_SDK_GRPC_PORT / AGONES_SDK_HTTP_PORT are honored for Agones
	// compatibility — official Agones SDKs read them too.
	listen := flag.String("listen", "localhost:"+envOr("AGONES_SDK_GRPC_PORT", "9357"), "local SDK listen address (gRPC, arena + agones.dev services)")
	restListen := flag.String("listen-http", "localhost:"+envOr("AGONES_SDK_HTTP_PORT", "9358"), "local Agones REST listen address")
	gatewayURL := flag.String("gateway", os.Getenv("ARENA_GATEWAY_ENDPOINT"), "arena-api SDK Gateway endpoint (default $ARENA_GATEWAY_ENDPOINT)")
	gsID := flag.String("gameserver-id", os.Getenv("ARENA_GAMESERVER_ID"), "GameServer ID (default $ARENA_GAMESERVER_ID)")
	flag.Parse()

	// SIGTERM starts a drain (Spot interruption / scale-in) but keeps the
	// sidecar alive so the game can evacuate within the stopTimeout window;
	// ECS SIGKILLs when it ends. SIGINT exits (dev).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(ctx, cancel, log, *listen, *restListen, *gatewayURL, *gsID); err != nil {
		log.Error("arena-sidecar exited", "error", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func run(ctx context.Context, cancel context.CancelFunc, log *slog.Logger, listen, restListen, gatewayURL, gsID string) error {
	if gatewayURL == "" {
		return errors.New("-gateway (or ARENA_GATEWAY_ENDPOINT) is required")
	}
	if gsID == "" {
		return errors.New("-gameserver-id (or ARENA_GAMESERVER_ID) is required")
	}

	taskARN, err := sidecar.DiscoverTaskARN(ctx)
	if err != nil {
		// Continue without a task ARN; whether that's fatal depends on
		// whether the gateway requires sidecar identity verification.
		log.Warn("task metadata discovery failed", "error", err)
	}

	client := gatewayv1connect.NewSDKGatewayClient(httpClientFor(gatewayURL), gatewayURL)
	sc := sidecar.New(client, sidecar.Options{GameServerID: gsID, TaskARN: taskARN}, log)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGTERM {
				// Infra drain: mark Draining upstream, stay alive for the
				// stopTimeout window.
				log.Info("SIGTERM: draining gameserver, staying up for the grace window")
				drainCtx, done := context.WithTimeout(ctx, 5*time.Second)
				if err := sc.Drain(drainCtx, "sigterm"); err != nil {
					log.Warn("drain request failed", "error", err)
				}
				done()
				continue
			}
			cancel() // SIGINT: exit now (dev)
			return
		}
	}()

	// One gRPC mux serves both the arena SDK and the Agones wire-compatible
	// services; connect handles the gRPC protocol over h2c.
	mux := http.NewServeMux()
	mux.Handle(arenav1connect.NewSDKHandler(sidecar.NewSDKServer(sc)))
	mux.Handle(sdkconnect.NewSDKHandler(sidecar.NewAgonesServer(sc)))
	mux.Handle(betaconnect.NewSDKHandler(sidecar.NewAgonesBetaServer(sc)))
	srv := &http.Server{
		Addr:    listen,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}
	restSrv := &http.Server{
		Addr:    restListen,
		Handler: sidecar.NewAgonesRESTHandler(sc),
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = restSrv.Shutdown(shutdownCtx)
	}()
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		if err := restSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	log.Info("arena-sidecar running", "listen", listen, "rest", restListen, "gateway", gatewayURL, "gameserver_id", gsID)
	go func() {
		errCh <- sc.Run(ctx) // returns nil on ctx cancellation
	}()
	return <-errCh
}

// httpClientFor returns an HTTP/2 client for the gateway: h2c for plaintext
// endpoints (bidirectional streams need HTTP/2, which net/http does not
// negotiate without TLS), the default client for https.
func httpClientFor(url string) *http.Client {
	if strings.HasPrefix(url, "https://") {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}
