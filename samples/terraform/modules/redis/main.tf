# ElastiCache Redis holds derived data only (ready pool, heartbeat TTLs,
# allocation pub/sub). Everything is rebuildable from DynamoDB via the epoch
# mechanism (DESIGN_v2 §8.2), so snapshots are disabled by design.

resource "aws_elasticache_subnet_group" "this" {
  name       = "${var.name_prefix}-redis"
  subnet_ids = var.subnet_ids

  tags = var.tags
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name_prefix}-redis"
  description          = "arena derived data (ready pool / heartbeats / pubsub)"

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type

  num_cache_clusters         = var.num_cache_nodes
  automatic_failover_enabled = var.automatic_failover_enabled
  multi_az_enabled           = var.multi_az_enabled

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = var.security_group_ids

  # Rebuildable from DynamoDB — no snapshots needed (DESIGN_v2 §13.3).
  snapshot_retention_limit = 0

  at_rest_encryption_enabled = true
  transit_encryption_enabled = false # in-VPC only; enable with TLS client support

  apply_immediately = false

  tags = var.tags
}
