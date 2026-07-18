package sdk

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/emptypb"

	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/gen/arena/v1/arenav1connect"
)

// DefaultAddress is the sidecar's local SDK endpoint.
const DefaultAddress = "http://localhost:9357"

// addressEnv overrides the sidecar address (local development, tests).
const addressEnv = "ARENA_SDK_ADDRESS"

// Client talks to the arena SDK sidecar. All methods are safe for
// concurrent use.
type Client struct {
	sdk arenav1connect.SDKClient
}

// New returns a Client for the sidecar at DefaultAddress (or
// $ARENA_SDK_ADDRESS when set). It does not dial eagerly; the first call
// does.
func New() *Client {
	addr := os.Getenv(addressEnv)
	if addr == "" {
		addr = DefaultAddress
	}
	return NewForAddress(addr)
}

// NewForAddress returns a Client for an explicit sidecar address.
func NewForAddress(addr string) *Client {
	return &Client{sdk: arenav1connect.NewSDKClient(httpClientFor(addr), addr, connect.WithGRPC())}
}

// Ready marks this game server Ready to accept allocations.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.sdk.Ready(ctx, connect.NewRequest(&emptypb.Empty{}))
	return err
}

// Health reports the game loop is alive. Call it at least every few seconds;
// once Health has been called, silence beyond the sidecar's health timeout
// marks the server Unhealthy.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.sdk.Health(ctx, connect.NewRequest(&emptypb.Empty{}))
	return err
}

// Shutdown signals the game server is done; the controller stops the task.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.sdk.Shutdown(ctx, connect.NewRequest(&emptypb.Empty{}))
	return err
}

// GameServer returns the current GameServer state (address, ports, labels,
// allocation status).
func (c *Client) GameServer(ctx context.Context) (*arenav1.GameServer, error) {
	res, err := c.sdk.GetGameServer(ctx, connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// SetLabel sets a metadata label (visible to allocation selectors).
func (c *Client) SetLabel(ctx context.Context, key, value string) error {
	_, err := c.sdk.SetLabel(ctx, connect.NewRequest(&arenav1.KeyValue{Key: key, Value: value}))
	return err
}

// SetAnnotation sets a metadata annotation.
func (c *Client) SetAnnotation(ctx context.Context, key, value string) error {
	_, err := c.sdk.SetAnnotation(ctx, connect.NewRequest(&arenav1.KeyValue{Key: key, Value: value}))
	return err
}

// WatchGameServer invokes fn for every GameServer update (including
// allocation pushes) until ctx is done or the stream fails. Typical use is
// waiting for STATE_ALLOCATED to learn the session metadata.
func (c *Client) WatchGameServer(ctx context.Context, fn func(*arenav1.GameServer)) error {
	stream, err := c.sdk.WatchGameServer(ctx, connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		return err
	}
	defer stream.Close()
	for stream.Receive() {
		fn(stream.Msg())
	}
	if err := stream.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// httpClientFor returns an HTTP/2 client: h2c for the plaintext localhost
// sidecar (gRPC needs HTTP/2, which net/http will not negotiate without
// TLS), the default client for https.
func httpClientFor(addr string) *http.Client {
	if strings.HasPrefix(addr, "https://") {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, a)
			},
		},
	}
}
