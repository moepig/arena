// Package gateway implements the SDK Gateway: it terminates the sidecar
// bidirectional streams and performs state transitions and heartbeats on the
// sidecars' behalf. Sidecars never touch DynamoDB or Redis.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	"github.com/moepig/arena/gen/arena/gateway/v1/gatewayv1connect"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
	"github.com/moepig/arena/internal/convert"
	"github.com/moepig/arena/internal/pool"
	"github.com/moepig/arena/internal/store"
)

// Store is the DynamoDB surface the gateway needs.
type Store interface {
	GetGameServer(ctx context.Context, gsID string) (*store.GameServer, error)
	TransitionState(ctx context.Context, gsID string, from, to store.State, mutate func(*store.GameServer)) (*store.GameServer, error)
	UpdateGameServerMetadata(ctx context.Context, gsID string, mutate func(*store.GameServer)) (*store.GameServer, error)
	SelfAllocateGameServer(ctx context.Context, gsID string, alloc store.Allocation) (*store.GameServer, error)
	ReleaseActiveAllocationsForGameServer(ctx context.Context, gsID string) error
}

// Pool is the Redis surface the gateway needs. SubscribeAllocation returns
// a payload channel plus a cancel func releasing the subscription.
type Pool interface {
	Add(ctx context.Context, fleetID, gsID string, score float64, labels map[string]string) error
	Remove(ctx context.Context, fleetID, gsID string) error
	SetHeartbeat(ctx context.Context, gsID string, now time.Time) error
	SubscribeAllocation(ctx context.Context, gsID string) (<-chan []byte, func())
	// SetCounters persists the sidecar's Counter/List primary-copy snapshot.
	SetCounters(ctx context.Context, fleetID, gsID string, counters map[string]pool.Counter, lists map[string]pool.List) error
}

// Verifier authenticates a sidecar's claimed identity. ecs.TaskVerifier
// checks the Task-ARN ⇄ startedBy binding; AcceptAll accepts every session
// unconditionally.
type Verifier interface {
	Verify(ctx context.Context, gameserverID, taskARN string) error
}

// AcceptAll is a Verifier that accepts every session unconditionally.
type AcceptAll struct{}

// Verify accepts every session.
func (AcceptAll) Verify(context.Context, string, string) error { return nil }

// Server implements arena.gateway.v1.SDKGateway.
type Server struct {
	gatewayv1connect.UnimplementedSDKGatewayHandler
	store    Store
	pool     Pool
	verifier Verifier
	log      *slog.Logger
}

// New returns a gateway server. verifier may be nil (accept all).
func New(s Store, p Pool, verifier Verifier, log *slog.Logger) *Server {
	if verifier == nil {
		verifier = AcceptAll{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: s, pool: p, verifier: verifier, log: log}
}

// Session terminates one sidecar stream. Every SidecarMessage carries
// gameserver_id; the first also authenticates the session. Acks answer
// requests in order; state pushes (WatchGameServer, allocation
// notifications) interleave from the pub/sub subscription.
func (g *Server) Session(ctx context.Context, stream *connect.BidiStream[gatewayv1.SidecarMessage, gatewayv1.GatewayMessage]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	gsID := first.GetGameserverId()
	if gsID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("first message must carry gameserver_id"))
	}
	if err := g.verifier.Verify(ctx, gsID, first.GetTaskArn()); err != nil {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("session verification failed: %w", err))
	}
	log := g.log.With("gameserver_id", gsID)
	log.Info("sidecar session opened")
	defer log.Info("sidecar session closed")

	ctx, cancel := context.WithCancel(ctx)

	// One writer goroutine owns stream.Send; acks and pushes are queued so
	// they never interleave a single send. Session must not return while a
	// Send is in flight (the handler closes the stream on return), so the
	// writer is joined on the way out.
	sends := make(chan *gatewayv1.GatewayMessage, 16)
	writerDone := make(chan struct{})
	defer func() {
		cancel()
		<-writerDone
	}()
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-sends:
				if err := stream.Send(msg); err != nil {
					cancel() // the broken stream also fails Receive below
					return
				}
			}
		}
	}()

	go g.pushLoop(ctx, gsID, sends, log)

	// Reconnect recovery: push the current state from DynamoDB so a missed
	// allocation notification is never lost. Also seeds fleetID, needed for
	// the Counter aux ZSET key but otherwise absent from the sidecar's
	// messages.
	var fleetID string
	if gs, err := g.store.GetGameServer(ctx, gsID); err == nil {
		fleetID = gs.FleetID
		g.trySend(sends, stateMsg(convert.GameServerToProto(gs)))
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	msg := first
	for {
		if err := g.handle(ctx, gsID, &fleetID, msg, sends, log); err != nil {
			return err
		}
		msg, err = stream.Receive()
		if err != nil {
			return err // io.EOF on clean sidecar close
		}
		if msg.GetGameserverId() != gsID {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("gameserver_id changed mid-session"))
		}
	}
}

func stateMsg(gs *arenav1.GameServer) *gatewayv1.GatewayMessage {
	return &gatewayv1.GatewayMessage{Msg: &gatewayv1.GatewayMessage_State{State: gs}}
}

func ackMsg() *gatewayv1.GatewayMessage {
	return &gatewayv1.GatewayMessage{Msg: &gatewayv1.GatewayMessage_Ack{Ack: &gatewayv1.Ack{}}}
}

// handle processes one sidecar request and queues the response. fleetID is a
// per-session cache (see Session); handleCounters lazily fills it in if the
// initial GetGameServer missed.
func (g *Server) handle(ctx context.Context, gsID string, fleetID *string, msg *gatewayv1.SidecarMessage, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) error {
	switch m := msg.GetMsg().(type) {
	case *gatewayv1.SidecarMessage_Heartbeat:
		// Redis only — never DynamoDB. Fire-and-forget, no ack; a lost
		// heartbeat is covered by the next one.
		if err := g.pool.SetHeartbeat(ctx, gsID, time.Now()); err != nil {
			log.Warn("heartbeat write failed", "error", err)
		}
		return nil

	case *gatewayv1.SidecarMessage_Ready:
		return g.handleReady(ctx, gsID, sends, log)

	case *gatewayv1.SidecarMessage_Shutdown:
		if err := g.handleShutdown(ctx, gsID, m.Shutdown.GetReason(), sends, log); err != nil {
			return err
		}
		g.trySend(sends, ackMsg())
		return nil

	case *gatewayv1.SidecarMessage_SetMetadata:
		return g.handleSetMetadata(ctx, gsID, m.SetMetadata, sends)

	case *gatewayv1.SidecarMessage_Reserve:
		return g.handleReserve(ctx, gsID, m.Reserve.GetSeconds(), sends, log)

	case *gatewayv1.SidecarMessage_Allocate:
		return g.handleSelfAllocate(ctx, gsID, sends, log)

	case *gatewayv1.SidecarMessage_Counters:
		if err := g.handleCounters(ctx, gsID, fleetID, m.Counters); err != nil {
			log.Warn("counters sync failed", "error", err)
		}
		return nil

	default:
		// Bare hello (id + task ARN only): nothing to do.
		return nil
	}
}

// handleReady commits a transition to Ready, then pools the server — in
// that order, or the pool could hand out a server whose state is not yet
// Ready. Three legal sources: Starting (initial Ready), Allocated (return to
// the pool for reuse; active allocation records are released), Reserved (end
// the reservation early). Any other state reports the current state instead
// of failing the stream.
func (g *Server) handleReady(ctx context.Context, gsID string, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) error {
	cur, err := g.store.GetGameServer(ctx, gsID)
	if err != nil {
		return err
	}
	switch cur.State {
	case store.StateStarting, store.StateAllocated, store.StateReserved:
	default:
		// Ready() retry, or the server already moved on. Report the
		// current state instead of failing the stream.
		g.trySend(sends, stateMsg(convert.GameServerToProto(cur)))
		return nil
	}
	if cur.State == store.StateAllocated {
		// Reuse path: close the allocation records first. Best-effort — the
		// authoritative reuse signal is the state transition below.
		if err := g.store.ReleaseActiveAllocationsForGameServer(ctx, gsID); err != nil {
			log.Warn("release allocations on ready failed", "error", err)
		}
	}
	gs, err := g.store.TransitionState(ctx, gsID, cur.State, store.StateReady, nil)
	if errors.Is(err, store.ErrConditionFailed) {
		latest, gerr := g.store.GetGameServer(ctx, gsID)
		if gerr != nil {
			return gerr
		}
		g.trySend(sends, stateMsg(convert.GameServerToProto(latest)))
		return nil
	}
	if err != nil {
		return err
	}
	if err := g.pool.Add(ctx, gs.FleetID, gs.ID, float64(gs.ReadyAt), gs.Labels); err != nil {
		// state=Ready but not pooled: the health reconciler's resync
		// detects "Ready but absent from pool" and re-adds (self-healing).
		log.Warn("ready pool add failed; resync will repair", "error", err)
	}
	log.Info("gameserver ready", "fleet_id", gs.FleetID, "from", string(cur.State))
	g.trySend(sends, stateMsg(convert.GameServerToProto(gs)))
	return nil
}

// handleReserve commits Ready|Reserved → Reserved with the requested window
// (seconds == 0 reserves until the SDK moves the server on), then removes
// the server from the ready pool. A stale pool entry is harmless either way:
// the allocator's conditional claim rejects non-Ready servers. The
// controller sweeps expired reservations back to Ready (reserved_until).
func (g *Server) handleReserve(ctx context.Context, gsID string, seconds int64, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) error {
	cur, err := g.store.GetGameServer(ctx, gsID)
	if err != nil {
		return err
	}
	switch cur.State {
	case store.StateReady, store.StateReserved:
	default:
		g.trySend(sends, stateMsg(convert.GameServerToProto(cur)))
		return nil
	}
	var until int64
	if seconds > 0 {
		until = time.Now().Add(time.Duration(seconds) * time.Second).Unix()
	}
	gs, err := g.store.TransitionState(ctx, gsID, cur.State, store.StateReserved, func(gs *store.GameServer) {
		gs.ReservedUntil = until
	})
	if errors.Is(err, store.ErrConditionFailed) {
		latest, gerr := g.store.GetGameServer(ctx, gsID)
		if gerr != nil {
			return gerr
		}
		g.trySend(sends, stateMsg(convert.GameServerToProto(latest)))
		return nil
	}
	if err != nil {
		return err
	}
	if cur.State == store.StateReady {
		if err := g.pool.Remove(ctx, gs.FleetID, gsID); err != nil {
			log.Warn("pool remove on reserve failed", "error", err)
		}
	}
	log.Info("gameserver reserved", "fleet_id", gs.FleetID, "reserved_until", until)
	g.trySend(sends, stateMsg(convert.GameServerToProto(gs)))
	return nil
}

// handleSelfAllocate commits Ready|Reserved → Allocated on the server's own
// initiative, synthesizing the Allocation record in the same transaction.
// The pool entry is removed afterwards; a stale entry is rejected by the
// allocator's conditional claim anyway.
func (g *Server) handleSelfAllocate(ctx context.Context, gsID string, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) error {
	alloc := store.Allocation{
		ID:       uuid.NewString(),
		Metadata: map[string]string{"arena.dev/self-allocated": "true"},
	}
	gs, err := g.store.SelfAllocateGameServer(ctx, gsID, alloc)
	if errors.Is(err, store.ErrConditionFailed) {
		latest, gerr := g.store.GetGameServer(ctx, gsID)
		if gerr != nil {
			return gerr
		}
		g.trySend(sends, stateMsg(convert.GameServerToProto(latest)))
		return nil
	}
	if err != nil {
		return err
	}
	if err := g.pool.Remove(ctx, gs.FleetID, gsID); err != nil {
		log.Warn("pool remove on self-allocate failed", "error", err)
	}
	log.Info("gameserver self-allocated", "fleet_id", gs.FleetID, "allocation_id", alloc.ID)
	g.trySend(sends, stateMsg(convert.GameServerToProto(gs)))
	return nil
}

// handleShutdown marks the server Draining so the controller stops the task
// and the fleet reconciler replaces it. Meaningful from
// Ready/Allocated/Reserved only; other states are already on their way out.
// The resulting state is pushed so WatchGameServer sees Draining — that is
// how an infrastructure drain (Spot SIGTERM) tells the game to evacuate the
// session within the grace window.
func (g *Server) handleShutdown(ctx context.Context, gsID, reason string, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) error {
	gs, err := g.store.GetGameServer(ctx, gsID)
	if err != nil {
		return err
	}
	switch gs.State {
	case store.StateReady, store.StateAllocated, store.StateReserved:
		ngs, err := g.store.TransitionState(ctx, gsID, gs.State, store.StateDraining, nil)
		if err != nil && !errors.Is(err, store.ErrConditionFailed) {
			return err
		}
		if err == nil {
			g.trySend(sends, stateMsg(convert.GameServerToProto(ngs)))
		}
		if gs.State == store.StateReady {
			if err := g.pool.Remove(ctx, gs.FleetID, gsID); err != nil {
				log.Warn("pool remove on shutdown failed", "error", err)
			}
		}
	}
	log.Info("gameserver shutdown requested", "state", string(gs.State), "reason", reason)
	return nil
}

// handleSetMetadata applies SetLabel / SetAnnotation and replies with the
// updated state.
func (g *Server) handleSetMetadata(ctx context.Context, gsID string, req *gatewayv1.SetMetadataRequest, sends chan<- *gatewayv1.GatewayMessage) error {
	gs, err := g.store.UpdateGameServerMetadata(ctx, gsID, func(gs *store.GameServer) {
		switch req.GetKind() {
		case gatewayv1.SetMetadataRequest_KIND_LABEL:
			if gs.Labels == nil {
				gs.Labels = map[string]string{}
			}
			gs.Labels[req.GetKey()] = req.GetValue()
		case gatewayv1.SetMetadataRequest_KIND_ANNOTATION:
			if gs.Annotations == nil {
				gs.Annotations = map[string]string{}
			}
			gs.Annotations[req.GetKey()] = req.GetValue()
		}
	})
	if err != nil {
		return err
	}
	g.trySend(sends, stateMsg(convert.GameServerToProto(gs)))
	return nil
}

// handleCounters persists a Counter/List full-state sync. No ack and no
// state push: the sidecar's copy is authoritative and this is
// a fire-and-forget Redis write. fleetID is resolved and cached on first use
// if the session's reconnect-recovery lookup missed it.
func (g *Server) handleCounters(ctx context.Context, gsID string, fleetID *string, sync *gatewayv1.CountersSync) error {
	if *fleetID == "" {
		gs, err := g.store.GetGameServer(ctx, gsID)
		if err != nil {
			return err
		}
		*fleetID = gs.FleetID
	}
	counters := make(map[string]pool.Counter, len(sync.GetCounters()))
	for name, c := range sync.GetCounters() {
		counters[name] = pool.Counter{Count: c.GetCount(), Capacity: c.GetCapacity()}
	}
	lists := make(map[string]pool.List, len(sync.GetLists()))
	for name, l := range sync.GetLists() {
		lists[name] = pool.List{Capacity: l.GetCapacity(), Values: l.GetValues()}
	}
	return g.pool.SetCounters(ctx, *fleetID, gsID, counters, lists)
}

// pushLoop forwards allocation pub/sub payloads to the stream writer.
func (g *Server) pushLoop(ctx context.Context, gsID string, sends chan<- *gatewayv1.GatewayMessage, log *slog.Logger) {
	ch, cancel := g.pool.SubscribeAllocation(ctx, gsID)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-ch:
			if !ok {
				return
			}
			gs, err := convert.DecodeStatePush(payload)
			if err != nil {
				log.Warn("bad allocation push payload", "error", err)
				continue
			}
			g.trySend(sends, stateMsg(gs))
		}
	}
}

// trySend queues a message without blocking session handling; a full queue
// drops the push (at-most-once — reconnect recovers).
func (g *Server) trySend(sends chan<- *gatewayv1.GatewayMessage, msg *gatewayv1.GatewayMessage) {
	select {
	case sends <- msg:
	default:
	}
}
