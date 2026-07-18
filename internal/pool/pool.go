// Package pool owns the Redis-side derived data: the ready
// pool sorted sets (pool:{epoch}:{fleet_id}), heartbeat keys with TTL
// (hb:{gameserver_id}), and the allocation push pub/sub channels
// (alloc:{gameserver_id}). Everything here is reconstructable from DynamoDB;
// losing Redis degrades performance, never correctness.
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// epochKey holds the current pool generation. Bumped by the controller
	// after a Redis failover to invalidate stale pools.
	epochKey = "pool:epoch"
	// epochChannel notifies arena-api instances of epoch changes.
	epochChannel = "pool:epoch:changed"

	// HeartbeatTTL is 3× the sidecar heartbeat interval (10s).
	HeartbeatTTL = 30 * time.Second
)

// ErrEmpty is returned by PopMin when the fleet has no Ready servers.
var ErrEmpty = errors.New("pool: no ready gameserver")

// Pool wraps the Redis derived-data structures. The current epoch is cached
// locally (atomic) and refreshed via pub/sub + Sync.
type Pool struct {
	rdb   redis.UniversalClient
	epoch atomic.Int64
}

// New returns a Pool. Call Sync before first use to load the epoch.
func New(rdb redis.UniversalClient) *Pool {
	p := &Pool{rdb: rdb}
	p.epoch.Store(1)
	return p
}

// Sync loads the current epoch from Redis, initializing it to 1 when absent.
func (p *Pool) Sync(ctx context.Context) error {
	// SETNX first so a fresh Redis starts at epoch 1 without a race.
	if err := p.rdb.SetNX(ctx, epochKey, 1, 0).Err(); err != nil {
		return err
	}
	v, err := p.rdb.Get(ctx, epochKey).Int64()
	if err != nil {
		return err
	}
	p.epoch.Store(v)
	return nil
}

// Epoch returns the locally cached pool generation.
func (p *Pool) Epoch() int64 { return p.epoch.Load() }

// Ping probes Redis liveness (pool rebuilder).
func (p *Pool) Ping(ctx context.Context) error { return p.rdb.Ping(ctx).Err() }

// BumpEpoch increments the pool generation and broadcasts the change.
// Old-epoch keys become garbage and expire from disuse; readers switch on
// the pub/sub notification (or their next Sync).
func (p *Pool) BumpEpoch(ctx context.Context) (int64, error) {
	v, err := p.rdb.Incr(ctx, epochKey).Result()
	if err != nil {
		return 0, err
	}
	p.epoch.Store(v)
	if err := p.rdb.Publish(ctx, epochChannel, strconv.FormatInt(v, 10)).Err(); err != nil {
		return v, err
	}
	return v, nil
}

// WatchEpoch keeps the cached epoch fresh until ctx is done. It combines the
// pub/sub channel (fast path) with a periodic Sync (safety net after missed
// messages / reconnects).
func (p *Pool) WatchEpoch(ctx context.Context, resync time.Duration) {
	sub := p.rdb.Subscribe(ctx, epochChannel)
	defer sub.Close()
	ch := sub.Channel()
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if v, err := strconv.ParseInt(msg.Payload, 10, 64); err == nil && v > p.epoch.Load() {
				p.epoch.Store(v)
			}
		case <-ticker.C:
			_ = p.Sync(ctx) // best-effort; next tick retries
		}
	}
}

func (p *Pool) key(fleetID string) string {
	return fmt.Sprintf("pool:%d:%s", p.epoch.Load(), fleetID)
}

func (p *Pool) selKey(fleetID, selHash string) string {
	return fmt.Sprintf("pool:%d:%s:sel:%s", p.epoch.Load(), fleetID, selHash)
}

// SelectorHash derives the selector sub-pool key hash for a label set.
// Deterministic across processes; "" for an empty set (no sub-pool).
func SelectorHash(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(labels[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Add puts a Ready GameServer into its fleet's pool — and, when it carries
// labels, into the selector sub-pool for that exact label set, which serves
// label-only selector allocations without a DynamoDB query.
// Score is ready_at, so ZPOPMIN allocates oldest-first (FIFO).
func (p *Pool) Add(ctx context.Context, fleetID, gsID string, score float64, labels map[string]string) error {
	z := redis.Z{Score: score, Member: gsID}
	if h := SelectorHash(labels); h != "" {
		pipe := p.rdb.Pipeline()
		pipe.ZAdd(ctx, p.key(fleetID), z)
		pipe.ZAdd(ctx, p.selKey(fleetID, h), z)
		_, err := pipe.Exec(ctx)
		return err
	}
	return p.rdb.ZAdd(ctx, p.key(fleetID), z).Err()
}

// PopMin atomically takes the next allocation candidate. Returns ErrEmpty
// when the pool has no Ready servers.
func (p *Pool) PopMin(ctx context.Context, fleetID string) (string, error) {
	return p.popMin(ctx, p.key(fleetID))
}

// PopMinSelector takes the next candidate from the sub-pool of servers
// pooled with exactly this label set. ErrEmpty on a miss — the caller falls
// back to the GSI slow path, so a cold or stale sub-pool costs correctness
// nothing. Entries may be stale (labels changed, server left Ready); the
// caller re-verifies before claiming.
func (p *Pool) PopMinSelector(ctx context.Context, fleetID string, labels map[string]string) (string, error) {
	h := SelectorHash(labels)
	if h == "" {
		return "", ErrEmpty
	}
	return p.popMin(ctx, p.selKey(fleetID, h))
}

func (p *Pool) popMin(ctx context.Context, key string) (string, error) {
	zs, err := p.rdb.ZPopMin(ctx, key, 1).Result()
	if err != nil {
		return "", err
	}
	if len(zs) == 0 {
		return "", ErrEmpty
	}
	return zs[0].Member.(string), nil
}

// Remove deletes a GameServer from its fleet's pool (Unhealthy / Draining /
// selector-path claim).
func (p *Pool) Remove(ctx context.Context, fleetID, gsID string) error {
	return p.rdb.ZRem(ctx, p.key(fleetID), gsID).Err()
}

// Size returns the number of pooled Ready servers for a fleet (metrics).
func (p *Pool) Size(ctx context.Context, fleetID string) (int64, error) {
	return p.rdb.ZCard(ctx, p.key(fleetID)).Result()
}

// Contains reports whether a GameServer is currently pooled (health
// reconciler's "Ready but absent from pool" repair).
func (p *Pool) Contains(ctx context.Context, fleetID, gsID string) (bool, error) {
	_, err := p.rdb.ZScore(ctx, p.key(fleetID), gsID).Result()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func hbKey(gsID string) string { return "hb:" + gsID }

// SetHeartbeat records liveness with a TTL. Heartbeats never touch DynamoDB.
func (p *Pool) SetHeartbeat(ctx context.Context, gsID string, now time.Time) error {
	return p.rdb.Set(ctx, hbKey(gsID), now.Unix(), HeartbeatTTL).Err()
}

// Heartbeats bulk-checks liveness via MGET; result[i] is true when ids[i]
// has an unexpired heartbeat.
func (p *Pool) Heartbeats(ctx context.Context, ids []string) ([]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = hbKey(id)
	}
	vals, err := p.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	alive := make([]bool, len(ids))
	for i, v := range vals {
		alive[i] = v != nil
	}
	return alive, nil
}

func allocChannel(gsID string) string { return "alloc:" + gsID }

// PublishAllocation pushes an allocation payload toward the arena-api
// instance holding the sidecar's stream. At-most-once by design: a missed
// message is recovered on sidecar reconnect from DynamoDB.
func (p *Pool) PublishAllocation(ctx context.Context, gsID string, payload []byte) error {
	return p.rdb.Publish(ctx, allocChannel(gsID), payload).Err()
}

// SubscribeAllocation subscribes to a GameServer's allocation channel and
// returns the payload stream plus a cancel func releasing the subscription.
// The channel is buffered and lossy (at-most-once semantics).
func (p *Pool) SubscribeAllocation(ctx context.Context, gsID string) (<-chan []byte, func()) {
	sub := p.rdb.Subscribe(ctx, allocChannel(gsID))
	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		for msg := range sub.Channel() {
			select {
			case out <- []byte(msg.Payload):
			default: // drop rather than block; reconnect recovers
			}
		}
	}()
	return out, func() { _ = sub.Close() }
}

// Counter is one named Counter's state. The game process
// is the source of truth; this is the Redis derived copy.
type Counter struct {
	Count, Capacity int64
}

// List is one named List's state.
type List struct {
	Capacity int64
	Values   []string
}

// Snapshot is one GameServer's full Counter/List state.
type Snapshot struct {
	Counters map[string]Counter
	Lists    map[string]List
}

func cntKey(gsID string) string { return "cnt:" + gsID }

func (p *Pool) cntAuxKey(fleetID, name string) string {
	return fmt.Sprintf("pool:%d:%s:cnt:%s", p.epoch.Load(), fleetID, name)
}

// SetCounters persists a GameServer's Counter/List primary-copy snapshot: a
// JSON blob at cnt:{gsID} for fleet-status aggregation, plus an aux ZSET per
// counter name (score = available = capacity-count) for the high-density
// allocation path. No TTL — the sidecar's 30s resend
// plus on-reconnect send keep it converged; a stale entry is only ever used
// as an accelerator, never a correctness source.
func (p *Pool) SetCounters(ctx context.Context, fleetID, gsID string, counters map[string]Counter, lists map[string]List) error {
	blob, err := json.Marshal(Snapshot{Counters: counters, Lists: lists})
	if err != nil {
		return err
	}
	pipe := p.rdb.Pipeline()
	pipe.Set(ctx, cntKey(gsID), blob, 0)
	for name, c := range counters {
		pipe.ZAdd(ctx, p.cntAuxKey(fleetID, name), redis.Z{Score: float64(c.Capacity - c.Count), Member: gsID})
	}
	_, err = pipe.Exec(ctx)
	return err
}

// Counters bulk-fetches Counter/List snapshots for the given GameServer ids
// (fleet aggregation into FleetStatus.counters). Ids with
// no stored snapshot (never reported one, or evicted) are omitted.
func (p *Pool) Counters(ctx context.Context, ids []string) (map[string]Snapshot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = cntKey(id)
	}
	vals, err := p.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]Snapshot, len(ids))
	for i, v := range vals {
		s, ok := v.(string)
		if !ok {
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(s), &snap); err != nil {
			continue
		}
		out[ids[i]] = snap
	}
	return out, nil
}

// ReserveCounter atomically claims `amount` of a GameServer's available
// Counter capacity by decrementing its score in the counter aux ZSET,
// provided enough remains — the concurrency guard for high-density
// reallocation: it closes the race between two concurrent
// Allocate calls deciding on the same snapshot before either's Allocation
// record commits. It is advisory only, since the Counter's source of truth
// is the game process, not this reservation; a missing
// entry (no Counter snapshot reported yet) fails closed (false, nil).
func (p *Pool) ReserveCounter(ctx context.Context, fleetID, name, gsID string, amount int64) (bool, error) {
	key := p.cntAuxKey(fleetID, name)
	ok := false
	err := p.rdb.Watch(ctx, func(tx *redis.Tx) error {
		score, err := tx.ZScore(ctx, key, gsID).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		if score < float64(amount) {
			return nil
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.ZIncrBy(ctx, key, -float64(amount), gsID)
			return nil
		})
		if err != nil {
			return err
		}
		ok = true
		return nil
	}, key)
	return ok, err
}

// ReleaseCounter reverses a ReserveCounter claim (allocation failure
// rollback, or Release restoring capacity).
func (p *Pool) ReleaseCounter(ctx context.Context, fleetID, name, gsID string, amount int64) error {
	return p.rdb.ZIncrBy(ctx, p.cntAuxKey(fleetID, name), float64(amount), gsID).Err()
}
