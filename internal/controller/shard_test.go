package controller

import "testing"

func TestFleetShardUnshardedAlwaysZero(t *testing.T) {
	for _, id := range []string{"f1", "f2", "anything"} {
		if got := FleetShard(id, 0); got != 0 {
			t.Errorf("FleetShard(%q, 0) = %d, want 0", id, got)
		}
		if got := FleetShard(id, 1); got != 0 {
			t.Errorf("FleetShard(%q, 1) = %d, want 0", id, got)
		}
	}
}

func TestFleetShardDeterministic(t *testing.T) {
	for _, id := range []string{"f1", "fleet-abc", "some-other-fleet-id"} {
		want := FleetShard(id, 8)
		for i := 0; i < 100; i++ {
			if got := FleetShard(id, 8); got != want {
				t.Fatalf("FleetShard(%q, 8) = %d on call %d, want stable %d", id, got, i, want)
			}
		}
	}
}

func TestFleetShardInRange(t *testing.T) {
	const shardCount = 4
	for i := 0; i < 1000; i++ {
		id := "fleet-" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		if s := FleetShard(id, shardCount); s < 0 || s >= shardCount {
			t.Fatalf("FleetShard(%q, %d) = %d, out of range", id, shardCount, s)
		}
	}
}

func TestFleetShardSpreadsAcrossShards(t *testing.T) {
	const shardCount = 4
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		id := "fleet-" + string(rune('a'+i%26)) + string(rune('A'+i%26)) + string(rune('0'+i%10))
		seen[FleetShard(id, shardCount)] = true
	}
	if len(seen) != shardCount {
		t.Errorf("shards touched = %d, want all %d exercised across 200 fleet ids", len(seen), shardCount)
	}
}
