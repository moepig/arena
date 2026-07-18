package sidecar

// Counters and Lists. The source of truth is the game
// server process: this in-memory store is the primary copy, every mutation
// ships the full state upstream (small, idempotent), and a 30s resend plus
// the on-reconnect send make the Redis copy rebuildable derived data.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
	arenav1 "github.com/moepig/arena/gen/arena/v1"
)

// Counter/List errors, mapped to Agones SDK codes by the beta service.
var (
	errNotFound      = errors.New("not found")
	errOutOfRange    = errors.New("out of range")
	errAlreadyExists = errors.New("already exists")
)

// Counter is the sidecar's view of one named counter.
type Counter struct {
	Count, Capacity int64
}

// List is the sidecar's view of one named list.
type List struct {
	Capacity int64
	Values   []string
}

// GetCounter returns a counter by name.
func (s *Sidecar) GetCounter(name string) (Counter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.counters[name]
	if !ok {
		return Counter{}, fmt.Errorf("counter %q: %w", name, errNotFound)
	}
	return c, nil
}

// UpdateCounter applies an Agones CounterUpdateRequest: optional absolute
// count/capacity plus a count diff, validated against [0, capacity].
// A counter unseen so far is created (arena has no spec-level counter
// declaration; the game process owns the set).
func (s *Sidecar) UpdateCounter(ctx context.Context, name string, count, capacity *int64, diff int64) (Counter, error) {
	s.mu.Lock()
	c := s.counters[name]
	if capacity != nil {
		if *capacity < 0 {
			s.mu.Unlock()
			return Counter{}, fmt.Errorf("capacity %d: %w", *capacity, errOutOfRange)
		}
		c.Capacity = *capacity
	}
	if count != nil {
		c.Count = *count
	}
	c.Count += diff
	if c.Count < 0 || c.Count > c.Capacity {
		s.mu.Unlock()
		return Counter{}, fmt.Errorf("count %d not in [0, %d]: %w", c.Count, c.Capacity, errOutOfRange)
	}
	s.counters[name] = c
	s.mu.Unlock()
	s.countersChanged(ctx)
	return c, nil
}

// GetList returns a list by name.
func (s *Sidecar) GetList(name string) (List, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lists[name]
	if !ok {
		return List{}, fmt.Errorf("list %q: %w", name, errNotFound)
	}
	return List{Capacity: l.Capacity, Values: slices.Clone(l.Values)}, nil
}

// UpdateList overwrites a list's capacity and/or values (Agones UpdateList
// semantics: values are replaced wholesale, truncated to capacity).
func (s *Sidecar) UpdateList(ctx context.Context, name string, capacity *int64, values []string, setValues bool) (List, error) {
	s.mu.Lock()
	l := s.lists[name]
	if capacity != nil {
		if *capacity < 0 {
			s.mu.Unlock()
			return List{}, fmt.Errorf("capacity %d: %w", *capacity, errOutOfRange)
		}
		l.Capacity = *capacity
	}
	if setValues {
		l.Values = slices.Clone(values)
	}
	if int64(len(l.Values)) > l.Capacity {
		l.Values = l.Values[:l.Capacity]
	}
	s.lists[name] = l
	s.mu.Unlock()
	s.countersChanged(ctx)
	return List{Capacity: l.Capacity, Values: slices.Clone(l.Values)}, nil
}

// AddListValue appends a value (ALREADY_EXISTS / OUT_OF_RANGE on capacity).
func (s *Sidecar) AddListValue(ctx context.Context, name, value string) (List, error) {
	s.mu.Lock()
	l, ok := s.lists[name]
	if !ok {
		s.mu.Unlock()
		return List{}, fmt.Errorf("list %q: %w", name, errNotFound)
	}
	if slices.Contains(l.Values, value) {
		s.mu.Unlock()
		return List{}, fmt.Errorf("value %q: %w", value, errAlreadyExists)
	}
	if int64(len(l.Values)) >= l.Capacity {
		s.mu.Unlock()
		return List{}, fmt.Errorf("list %q at capacity %d: %w", name, l.Capacity, errOutOfRange)
	}
	l.Values = append(slices.Clone(l.Values), value)
	s.lists[name] = l
	s.mu.Unlock()
	s.countersChanged(ctx)
	return List{Capacity: l.Capacity, Values: slices.Clone(l.Values)}, nil
}

// RemoveListValue removes a value (NOT_FOUND when absent).
func (s *Sidecar) RemoveListValue(ctx context.Context, name, value string) (List, error) {
	s.mu.Lock()
	l, ok := s.lists[name]
	if !ok {
		s.mu.Unlock()
		return List{}, fmt.Errorf("list %q: %w", name, errNotFound)
	}
	i := slices.Index(l.Values, value)
	if i < 0 {
		s.mu.Unlock()
		return List{}, fmt.Errorf("value %q: %w", value, errNotFound)
	}
	l.Values = slices.Delete(slices.Clone(l.Values), i, i+1)
	s.lists[name] = l
	s.mu.Unlock()
	s.countersChanged(ctx)
	return List{Capacity: l.Capacity, Values: slices.Clone(l.Values)}, nil
}

// countersChanged ships the full state upstream and pokes watchers so the
// Agones adapter serves fresh counter values on Watch.
func (s *Sidecar) countersChanged(ctx context.Context) {
	if msg := s.countersSyncMsg(); msg != nil {
		_ = s.send(ctx, msg)
	}
	s.mu.Lock()
	state := s.state
	watchers := make([]chan *arenav1.GameServer, 0, len(s.watchers))
	for _, ch := range s.watchers {
		watchers = append(watchers, ch)
	}
	s.mu.Unlock()
	if state == nil {
		return
	}
	for _, ch := range watchers {
		select {
		case ch <- state:
		default:
		}
	}
}

// countersSyncMsg snapshots the full Counter/List state (nil when empty).
func (s *Sidecar) countersSyncMsg() *gatewayv1.SidecarMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counters) == 0 && len(s.lists) == 0 {
		return nil
	}
	sync := &gatewayv1.CountersSync{
		Counters: make(map[string]*gatewayv1.CounterState, len(s.counters)),
		Lists:    make(map[string]*gatewayv1.ListState, len(s.lists)),
	}
	for name, c := range s.counters {
		sync.Counters[name] = &gatewayv1.CounterState{Count: c.Count, Capacity: c.Capacity}
	}
	for name, l := range s.lists {
		sync.Lists[name] = &gatewayv1.ListState{Capacity: l.Capacity, Values: slices.Clone(l.Values)}
	}
	return &gatewayv1.SidecarMessage{Msg: &gatewayv1.SidecarMessage_Counters{Counters: sync}}
}

// CounterSnapshot returns copies of the current counters and lists (Agones
// adapter status rendering).
func (s *Sidecar) CounterSnapshot() (map[string]Counter, map[string]List) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counters := make(map[string]Counter, len(s.counters))
	for k, v := range s.counters {
		counters[k] = v
	}
	lists := make(map[string]List, len(s.lists))
	for k, v := range s.lists {
		lists[k] = List{Capacity: v.Capacity, Values: slices.Clone(v.Values)}
	}
	return counters, lists
}

// countersResendLoop re-sends the full state every 30s — the recovery model
// for Redis failover: derived data converges from the
// primary copy without an epoch rebuild.
func (s *Sidecar) countersResendLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if msg := s.countersSyncMsg(); msg != nil {
				select {
				case s.outbox <- s.envelope(msg):
				default: // congested: the next tick retries
				}
			}
		}
	}
}
