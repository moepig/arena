package controller

import "hash/fnv"

// FleetShard maps a fleet ID onto one of shardCount shards: the same fleet
// always lands on the same shard regardless of which controller process is
// running or how many are, which is what
// lets independent per-shard leases split fleet reconciliation across
// controllers with no coordination beyond the lease table. shardCount is
// static operator configuration (Options.ShardCount) — it doesn't change at
// runtime — so a stable hash-mod is sufficient; a consistent-hashing ring's
// minimal-remapping property only pays for itself when the bucket count
// itself changes on the fly, which never happens here. shardCount <= 1
// always returns 0 (the unsharded case: everything is "shard 0").
func FleetShard(fleetID string, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(fleetID))
	return int(h.Sum32() % uint32(shardCount))
}
