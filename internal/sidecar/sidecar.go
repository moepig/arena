// Package sidecar implements the SDK sidecar core: one
// outbound SDKGateway stream multiplexing everything (Ready / heartbeat /
// metadata / watch), with exponential-backoff reconnect, plus the local
// Agones-compatible SDK server the game server container talks to. The
// sidecar has no access to DynamoDB, Redis, or the ECS API.
package sidecar

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

// Options configure a Sidecar.
type Options struct {
	// GameServerID is injected by the controller via the task's container
	// environment (ARENA_GAMESERVER_ID).
	GameServerID string
	// TaskARN is discovered from the ECS task metadata endpoint and lets the
	// gateway verify the claimed identity. May be empty
	// off-ECS (dev).
	TaskARN string
	// HeartbeatInterval is the upstream heartbeat cadence. Default 10s
	// (Redis TTL is 3× this).
	HeartbeatInterval time.Duration
	// HealthTimeout gates heartbeats on the game server's own Health()
	// calls: once the game has called Health() at least once, heartbeats
	// stop when no Health() arrives within this window, which is how a hung
	// game process becomes Unhealthy upstream.
	// Before the first Health() call heartbeats flow unconditionally
	// (startup grace is enforced server-side). Default 30s.
	HealthTimeout time.Duration
	// MinBackoff / MaxBackoff bound the reconnect backoff. Defaults 1s / 30s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (o *Options) defaults() {
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = 10 * time.Second
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 30 * time.Second
	}
	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 30 * time.Second
	}
}

// Sidecar owns the gateway session and the locally cached GameServer state.
type Sidecar struct {
	client gatewayv1connect.SDKGatewayClient
	opts   Options
	log    *slog.Logger

	// outbox carries requests to whichever session goroutine currently owns
	// the stream; it survives reconnects, so a Ready() sent while
	// disconnected goes out on the next session.
	outbox chan *gatewayv1.SidecarMessage

	mu       sync.Mutex
	state    *arenav1.GameServer
	watchers map[int]chan *arenav1.GameServer
	nextID   int
	// counters/lists: the primary Counter/List copy.
	counters map[string]Counter
	lists    map[string]List

	lastHealth atomic.Int64 // unix nanos of the last Health() call; 0 = never
	now        func() time.Time
}

// New returns a Sidecar talking to the given gateway client.
func New(client gatewayv1connect.SDKGatewayClient, opts Options, log *slog.Logger) *Sidecar {
	opts.defaults()
	if log == nil {
		log = slog.Default()
	}
	return &Sidecar{
		client:   client,
		opts:     opts,
		log:      log,
		outbox:   make(chan *gatewayv1.SidecarMessage, 64),
		watchers: map[int]chan *arenav1.GameServer{},
		counters: map[string]Counter{},
		lists:    map[string]List{},
		now:      time.Now,
	}
}

// Run maintains the gateway session until ctx is done: connect, pump, and on
// failure reconnect with exponential backoff (reset after a stable session).
func (s *Sidecar) Run(ctx context.Context) error {
	go s.heartbeatLoop(ctx)
	go s.countersResendLoop(ctx)

	backoff := s.opts.MinBackoff
	for {
		start := s.now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if s.now().Sub(start) >= time.Minute {
			backoff = s.opts.MinBackoff
		}
		s.log.Warn("gateway session ended; reconnecting", "error", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > s.opts.MaxBackoff {
			backoff = s.opts.MaxBackoff
		}
	}
}

// session runs one stream: hello, writer draining the outbox, reader
// applying state pushes. Returns when the stream breaks or ctx is done.
func (s *Sidecar) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream := s.client.Session(ctx)
	defer stream.CloseResponse()
	defer stream.CloseRequest()

	// Hello: identity only. The gateway answers with the current state from
	// DynamoDB, which is how a missed allocation push is recovered.
	if err := stream.Send(s.envelope(&gatewayv1.SidecarMessage{})); err != nil {
		return err
	}
	// Reconnect recovery for the Redis Counter copy: the primary state
	// lives here, so a fresh session re-seeds it.
	if msg := s.countersSyncMsg(); msg != nil {
		if err := stream.Send(s.envelope(msg)); err != nil {
			return err
		}
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-s.outbox:
				if err := stream.Send(msg); err != nil {
					s.requeue(msg)
					cancel() // fails Receive below
					return
				}
			}
		}
	}()

	for {
		msg, err := stream.Receive()
		if err != nil {
			return err
		}
		if gs := msg.GetState(); gs != nil {
			s.setState(gs)
		}
	}
}

// envelope stamps the session identity onto a message.
func (s *Sidecar) envelope(m *gatewayv1.SidecarMessage) *gatewayv1.SidecarMessage {
	m.GameserverId = s.opts.GameServerID
	m.TaskArn = s.opts.TaskARN
	return m
}

// send queues a message for the gateway, blocking only when the outbox is
// full (prolonged disconnect with a chatty caller).
func (s *Sidecar) send(ctx context.Context, m *gatewayv1.SidecarMessage) error {
	select {
	case s.outbox <- s.envelope(m):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Drain requests an infrastructure-initiated shutdown (Spot interruption /
// scale-in SIGTERM): the gateway moves the server to
// Draining and the game sees the new state via WatchGameServer, giving it
// the stopTimeout window to evacuate the session.
func (s *Sidecar) Drain(ctx context.Context, reason string) error {
	return s.send(ctx, &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Shutdown{Shutdown: &gatewayv1.ShutdownRequest{Reason: reason}},
	})
}

// requeue puts a message the broken stream failed to write back on the
// outbox so it is retried on reconnect. Best-effort: when the outbox is
// already full the message is dropped (heartbeats and metadata converge on
// their own; Ready() is retried by resync marking the server Unhealthy).
func (s *Sidecar) requeue(m *gatewayv1.SidecarMessage) {
	select {
	case s.outbox <- m:
	default:
		s.log.Warn("outbox full on reconnect; dropping message")
	}
}

// heartbeatLoop sends the 10s heartbeat while the game process is
// considered healthy. Heartbeats are skipped (not queued) when the outbox
// is congested — a stale heartbeat is worthless.
func (s *Sidecar) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(s.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.healthy() {
				s.log.Warn("no Health() within timeout; suppressing heartbeat")
				continue
			}
			select {
			case s.outbox <- s.envelope(&gatewayv1.SidecarMessage{
				Msg: &gatewayv1.SidecarMessage_Heartbeat{Heartbeat: &gatewayv1.Heartbeat{}},
			}):
			default:
			}
		}
	}
}

// healthy reports whether heartbeats should flow (see Options.HealthTimeout).
func (s *Sidecar) healthy() bool {
	last := s.lastHealth.Load()
	if last == 0 {
		return true
	}
	return s.now().Sub(time.Unix(0, last)) <= s.opts.HealthTimeout
}

// recordHealth notes a Health() call from the game server.
func (s *Sidecar) recordHealth() {
	s.lastHealth.Store(s.now().UnixNano())
}

// setState caches the latest GameServer and fans it out to watchers
// (lossy per watcher; each watcher channel keeps only the freshest lag).
func (s *Sidecar) setState(gs *arenav1.GameServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = gs
	for _, ch := range s.watchers {
		select {
		case ch <- gs:
		default:
		}
	}
}

// State returns the last known GameServer (nil before the first push).
func (s *Sidecar) State() *arenav1.GameServer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// watch registers a watcher channel; the current state (if any) is
// pre-loaded so new watchers see it immediately.
func (s *Sidecar) watch() (int, <-chan *arenav1.GameServer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	ch := make(chan *arenav1.GameServer, 8)
	if s.state != nil {
		ch <- s.state
	}
	s.watchers[id] = ch
	return id, ch
}

func (s *Sidecar) unwatch(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watchers, id)
}
