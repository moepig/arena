// arena-router is the allocation router: a stateless
// arena.v1.AllocationService front door that forwards to statically
// configured per-region arena-api deployments, trying regions in
// priority/weight order and falling back on RESOURCE_EXHAUSTED. It carries
// no state of its own — regional IAM auth applies unchanged, and Global
// Tables / cross-region consistency never enter the allocation hot path.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
	"github.com/moepig/arena/internal/router"
)

// regionConfig is one entry of the -config JSON file: a plain array of
// {name, endpoint, priority, weight} (a static policy).
type regionConfig struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Priority int32  `json:"priority"`
	Weight   int32  `json:"weight"`
}

func main() {
	addr := flag.String("listen", ":8090", "listen address")
	configPath := flag.String("config", os.Getenv("ARENA_ROUTER_CONFIG"), "path to the region policy JSON file (default $ARENA_ROUTER_CONFIG)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if err := run(*addr, *configPath, log); err != nil {
		log.Error("arena-router exited", "error", err)
		os.Exit(1)
	}
}

func run(addr, configPath string, log *slog.Logger) error {
	if configPath == "" {
		return errors.New("-config (or ARENA_ROUTER_CONFIG) is required")
	}
	regions, err := loadRegions(configPath)
	if err != nil {
		return err
	}
	for _, r := range regions {
		log.Info("region configured", "name", r.Name, "endpoint", r.Endpoint, "priority", r.Priority, "weight", r.Weight)
	}

	rt := router.New(regions, nil)
	mux := http.NewServeMux()
	mux.Handle(arenav1connect.NewAllocationServiceHandler(&router.Handler{Router: rt}))

	srv := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(mux, &http2.Server{}),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Info("arena-router running", "listen", addr, "regions", len(regions))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// loadRegions parses the region policy file and builds one AllocationService
// client per endpoint.
func loadRegions(path string) ([]router.Region, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfgs []regionConfig
	if err := json.Unmarshal(b, &cfgs); err != nil {
		return nil, err
	}
	if len(cfgs) == 0 {
		return nil, errors.New("region policy file has no entries")
	}
	regions := make([]router.Region, 0, len(cfgs))
	for i, c := range cfgs {
		if c.Name == "" {
			return nil, errRegionField(i, "name")
		}
		if c.Endpoint == "" {
			return nil, errRegionField(i, "endpoint")
		}
		regions = append(regions, router.Region{
			Name:     c.Name,
			Endpoint: c.Endpoint,
			Priority: c.Priority,
			Weight:   c.Weight,
			Client:   arenav1connect.NewAllocationServiceClient(httpClientFor(c.Endpoint), c.Endpoint),
		})
	}
	return regions, nil
}

func errRegionField(i int, field string) error {
	return errors.New("region policy[" + strconv.Itoa(i) + "]: " + field + " is required")
}

// httpClientFor returns an HTTP/2 client for a region endpoint: h2c for
// plaintext (arena-api's internal listener typically has no TLS in front of
// it — a regional load balancer terminates that), the default client for
// https.
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
