# Two services only (DESIGN_v2 §2.2):
#   arena-api        — stateless, ALB target, autoscaled
#   arena-controller — leader + hot standby (desired_count = 2), no LB

locals {
  container_port = 8080

  common_env = [
    { name = "ARENA_REDIS_ENDPOINT", value = var.redis_endpoint },
    { name = "ARENA_TABLE_FLEETS", value = var.dynamodb_table_names["fleets"] },
    { name = "ARENA_TABLE_GAMESERVERS", value = var.dynamodb_table_names["gameservers"] },
    { name = "ARENA_TABLE_ALLOCATIONS", value = var.dynamodb_table_names["allocations"] },
    { name = "ARENA_TABLE_LEASES", value = var.dynamodb_table_names["leases"] },
    { name = "ARENA_GAMESERVER_CLUSTER", value = var.gameserver_cluster_arn },
  ]
}

# --- Logs --------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "api" {
  name              = "/${var.name_prefix}/api"
  retention_in_days = 30

  tags = var.tags
}

resource "aws_cloudwatch_log_group" "controller" {
  name              = "/${var.name_prefix}/controller"
  retention_in_days = 30

  tags = var.tags
}

resource "aws_cloudwatch_log_group" "gameserver" {
  name              = "/${var.name_prefix}/gameserver"
  retention_in_days = 14

  tags = var.tags
}

# --- ALB (control API only; game traffic never goes through an LB) -----------

resource "aws_lb" "api" {
  name               = "${var.name_prefix}-api"
  load_balancer_type = "application"
  internal           = false
  subnets            = var.alb_subnet_ids
  security_groups    = [var.alb_security_group_id]

  tags = var.tags
}

resource "aws_lb_target_group" "api" {
  name        = "${var.name_prefix}-api"
  port        = local.container_port
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip"

  # gRPC/Connect streams: keep draining fast, sidecars reconnect.
  deregistration_delay = 30

  health_check {
    path                = "/healthz"
    interval            = 15
    healthy_threshold   = 2
    unhealthy_threshold = 3
    matcher             = "200"
  }

  tags = var.tags
}

resource "aws_lb_listener" "api" {
  load_balancer_arn = aws_lb.api.arn

  port            = var.certificate_arn == "" ? 80 : 443
  protocol        = var.certificate_arn == "" ? "HTTP" : "HTTPS"
  ssl_policy      = var.certificate_arn == "" ? null : "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn = var.certificate_arn == "" ? null : var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }

  tags = var.tags
}

# --- arena-api ----------------------------------------------------------------

resource "aws_ecs_task_definition" "api" {
  family                   = "${var.name_prefix}-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = var.task_execution_role_arn
  task_role_arn            = var.api_task_role_arn

  container_definitions = jsonencode([
    {
      name      = "arena-api"
      image     = var.api_image
      essential = true
      portMappings = [
        { containerPort = local.container_port, protocol = "tcp" }
      ]
      environment = local.common_env
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.api.name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = "api"
        }
      }
    }
  ])

  tags = var.tags
}

resource "aws_ecs_service" "api" {
  name            = "${var.name_prefix}-api"
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.api.arn
  desired_count   = var.api_desired_count
  launch_type     = "FARGATE"

  # Rolling update without capacity loss; sidecar streams reconnect.
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  network_configuration {
    subnets         = var.private_subnet_ids
    security_groups = [var.api_security_group_id]
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "arena-api"
    container_port   = local.container_port
  }

  lifecycle {
    ignore_changes = [desired_count] # owned by autoscaling
  }

  tags = var.tags
}

resource "aws_appautoscaling_target" "api" {
  service_namespace  = "ecs"
  resource_id        = "service/${var.cluster_name}/${aws_ecs_service.api.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.api_desired_count
  max_capacity       = var.api_max_count
}

resource "aws_appautoscaling_policy" "api_cpu" {
  name               = "${var.name_prefix}-api-cpu"
  service_namespace  = aws_appautoscaling_target.api.service_namespace
  resource_id        = aws_appautoscaling_target.api.resource_id
  scalable_dimension = aws_appautoscaling_target.api.scalable_dimension
  policy_type        = "TargetTrackingScaling"

  target_tracking_scaling_policy_configuration {
    target_value = 60

    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
  }
}

# --- arena-controller ----------------------------------------------------------

resource "aws_ecs_task_definition" "controller" {
  family                   = "${var.name_prefix}-controller"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 512
  memory                   = 1024
  execution_role_arn       = var.task_execution_role_arn
  task_role_arn            = var.controller_task_role_arn

  container_definitions = jsonencode([
    {
      name      = "arena-controller"
      image     = var.controller_image
      essential = true
      environment = concat(local.common_env, [
        { name = "ARENA_EVENTS_QUEUE_URL", value = var.events_queue_url },
      ])
      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.controller.name
          awslogs-region        = var.aws_region
          awslogs-stream-prefix = "controller"
        }
      }
    }
  ])

  tags = var.tags
}

# Leader + hot standby; election happens via the DynamoDB leases table
# (RTO ≈ lease TTL 15s).
resource "aws_ecs_service" "controller" {
  name            = "${var.name_prefix}-controller"
  cluster         = var.cluster_arn
  task_definition = aws_ecs_task_definition.controller.arn
  desired_count   = 2
  launch_type     = "FARGATE"

  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 150

  network_configuration {
    subnets         = var.private_subnet_ids
    security_groups = [var.controller_security_group_id]
  }

  tags = var.tags
}
