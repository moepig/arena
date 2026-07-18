package controller

import (
	"context"
	"errors"
	"time"

	"github.com/moepig/arena/internal/store"
)

// rebuildLoop watches Redis liveness and rebuilds the ready pools after a
// failover. Pools are derived data: losing Redis degrades
// allocation availability, never correctness, and this loop restores it.
func (c *Controller) rebuildLoop(ctx context.Context) {
	healthy := true // a startup outage triggers on its first recovery
	ticker := time.NewTicker(c.opts.RedisPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := c.pool.Ping(ctx)
		switch {
		case err != nil && healthy:
			healthy = false
			c.log.Warn("redis unreachable; pool rebuild armed", "error", err)
		case err == nil && !healthy:
			// Give sidecars two heartbeat cycles to repopulate hb: keys, or
			// the rebuild would drop live servers.
			c.log.Info("redis recovered; rebuilding pools", "delay", c.opts.RebuildDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(c.opts.RebuildDelay):
			}
			if err := c.RebuildPools(ctx); err != nil {
				c.log.Warn("pool rebuild failed; will retry on next recovery", "error", err)
				continue // stay unhealthy → retried on the next OK ping
			}
			healthy = true
		}
	}
}

// RebuildPools bumps the pool epoch (logically invalidating every old pool)
// and repopulates the new-epoch pools from the source of truth: fleet-index
// Ready servers that have a live heartbeat.
func (c *Controller) RebuildPools(ctx context.Context) error {
	epoch, err := c.pool.BumpEpoch(ctx)
	if err != nil {
		return err
	}
	c.log.Info("pool epoch bumped", "epoch", epoch)

	fleets, err := c.store.ListAllFleets(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for i := range fleets {
		errs = append(errs, c.rebuildFleetPool(ctx, fleets[i].ID))
		// Reconcile catches anything this pass missed (level trigger).
		c.queue.Add(fleets[i].ID)
	}
	return errors.Join(errs...)
}

func (c *Controller) rebuildFleetPool(ctx context.Context, fleetID string) error {
	ready, err := c.store.ListAllGameServersByFleet(ctx, fleetID, store.StateReady)
	if err != nil {
		return err
	}
	if len(ready) == 0 {
		return nil
	}
	ids := make([]string, len(ready))
	for i := range ready {
		ids[i] = ready[i].ID
	}
	alive, err := c.pool.Heartbeats(ctx, ids)
	if err != nil {
		return err
	}
	var errs []error
	restored := 0
	for i := range ready {
		if !alive[i] {
			continue // the health sweep decides its fate
		}
		if err := c.pool.Add(ctx, fleetID, ready[i].ID, float64(ready[i].ReadyAt), ready[i].Labels); err != nil {
			errs = append(errs, err)
			continue
		}
		restored++
	}
	c.log.Info("fleet pool rebuilt", "fleet_id", fleetID, "restored", restored, "ready", len(ready))
	return errors.Join(errs...)
}
