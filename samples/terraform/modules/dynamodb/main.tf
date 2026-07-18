# Four tables (DESIGN_v2 §3.1). All On-Demand. Deliberately absent:
#   - an autoscalers table (autoscaling is a Fleet attribute)
#   - any GSI with a global low-cardinality "state" partition key
#     (fleet-index scopes state lookups to a fleet instead)

resource "aws_dynamodb_table" "fleets" {
  name         = "${var.name_prefix}-fleets"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "fleet_id"

  attribute {
    name = "fleet_id"
    type = "S"
  }

  attribute {
    name = "namespace"
    type = "S"
  }

  attribute {
    name = "name"
    type = "S"
  }

  global_secondary_index {
    name            = "namespace-name-index"
    hash_key        = "namespace"
    range_key       = "name"
    projection_type = "ALL"
  }

  point_in_time_recovery {
    enabled = var.enable_point_in_time_recovery
  }

  tags = var.tags
}

resource "aws_dynamodb_table" "gameservers" {
  name         = "${var.name_prefix}-gameservers"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "gameserver_id"

  attribute {
    name = "gameserver_id"
    type = "S"
  }

  attribute {
    name = "fleet_id"
    type = "S"
  }

  # Composite "state#created_at" sort key: reconcile listing, selector
  # allocation and pool rebuild all use begins_with() on this one GSI.
  attribute {
    name = "state_created_at"
    type = "S"
  }

  global_secondary_index {
    name            = "fleet-index"
    hash_key        = "fleet_id"
    range_key       = "state_created_at"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  point_in_time_recovery {
    enabled = var.enable_point_in_time_recovery
  }

  tags = var.tags
}

resource "aws_dynamodb_table" "allocations" {
  name         = "${var.name_prefix}-allocations"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "allocation_id"

  attribute {
    name = "allocation_id"
    type = "S"
  }

  attribute {
    name = "session_id"
    type = "S"
  }

  attribute {
    name = "gameserver_id"
    type = "S"
  }

  attribute {
    name = "allocated_at"
    type = "N"
  }

  global_secondary_index {
    name            = "session-index"
    hash_key        = "session_id"
    projection_type = "ALL"
  }

  global_secondary_index {
    name            = "gameserver-index"
    hash_key        = "gameserver_id"
    range_key       = "allocated_at"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  point_in_time_recovery {
    enabled = var.enable_point_in_time_recovery
  }

  tags = var.tags
}

# Controller leader election (conditional-put lease, TTL 15s).
resource "aws_dynamodb_table" "leases" {
  name         = "${var.name_prefix}-leases"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "lease_name"

  attribute {
    name = "lease_name"
    type = "S"
  }

  point_in_time_recovery {
    enabled = var.enable_point_in_time_recovery
  }

  tags = var.tags
}
