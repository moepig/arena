# Two clusters: control plane and game servers are separated for quota
# isolation, blast radius, and so the EventBridge rule can filter on a single
# cluster ARN (plan §3.2).

resource "aws_ecs_cluster" "control_plane" {
  name = "${var.name_prefix}-control-plane"

  setting {
    name  = "containerInsights"
    value = var.enable_container_insights ? "enabled" : "disabled"
  }

  tags = var.tags
}

resource "aws_ecs_cluster" "gameserver" {
  name = "${var.name_prefix}-gameserver"

  setting {
    name  = "containerInsights"
    value = var.enable_container_insights ? "enabled" : "disabled"
  }

  tags = var.tags
}

resource "aws_ecs_cluster_capacity_providers" "control_plane" {
  cluster_name       = aws_ecs_cluster.control_plane.name
  capacity_providers = ["FARGATE"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

# FARGATE_SPOT is available for game servers: interruptions surface as
# STOPPED events and the reconciler replenishes automatically (DESIGN_v2 §10).
resource "aws_ecs_cluster_capacity_providers" "gameserver" {
  cluster_name       = aws_ecs_cluster.gameserver.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}
