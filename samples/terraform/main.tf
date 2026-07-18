terraform {
  required_version = ">= 1.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  # No v1 resources exist in this state (verify with `terraform state list`
  # before the first v2 apply — plan_v1_to_v2_migration.md §3.1).
  backend "s3" {
    bucket = "arena-terraform-state"
    key    = "infrastructure/terraform.tfstate"
    region = "us-west-2"
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = "arena"
      Environment = var.environment
      ManagedBy   = "Terraform"
    }
  }
}

locals {
  name_prefix = "arena-${var.environment}"

  common_tags = {
    Project     = "arena"
    Environment = var.environment
  }
}

# VPC and networking. No load balancer for game traffic: clients connect
# directly to task public IPs (DESIGN_v2 §6.1).
module "vpc" {
  source = "./modules/vpc"

  name_prefix        = local.name_prefix
  vpc_cidr           = var.vpc_cidr
  availability_zones = var.availability_zones

  allowed_cidrs         = var.allowed_cidrs
  gameserver_port_range = var.gameserver_port_range

  tags = local.common_tags
}

# DynamoDB: fleets / gameservers / allocations / leases. Single source of
# truth; no autoscalers table (autoscaling is a Fleet field).
module "dynamodb" {
  source = "./modules/dynamodb"

  name_prefix = local.name_prefix

  enable_point_in_time_recovery = var.enable_pitr

  tags = local.common_tags
}

# ElastiCache Redis: derived data only (ready pool / heartbeats / pub-sub),
# rebuildable from DynamoDB, so snapshots are disabled.
module "redis" {
  source = "./modules/redis"

  name_prefix = local.name_prefix

  subnet_ids         = module.vpc.private_subnet_ids
  security_group_ids = [module.vpc.redis_security_group_id]

  node_type       = var.redis_node_type
  num_cache_nodes = var.redis_num_nodes
  engine_version  = "7.0"

  automatic_failover_enabled = true
  multi_az_enabled           = true

  tags = local.common_tags
}

# ECS clusters: control plane and game servers are separate clusters
# (quotas, blast radius, and a simple EventBridge rule filter).
module "ecs" {
  source = "./modules/ecs"

  name_prefix = local.name_prefix

  enable_container_insights = true

  tags = local.common_tags
}

# IAM: three task roles (api / controller / gameserver). Game server tasks
# get CloudWatch Logs only — no DynamoDB/Redis/ECS access (DESIGN_v2 §6.3).
module "iam" {
  source = "./modules/iam"

  name_prefix = local.name_prefix

  dynamodb_table_arns    = module.dynamodb.table_arns
  events_queue_arn       = module.events.queue_arn
  gameserver_cluster_arn = module.ecs.gameserver_cluster_arn

  tags = local.common_tags
}

# EventBridge → SQS wiring for ECS task state change events (edge trigger).
module "events" {
  source = "./modules/events"

  name_prefix = local.name_prefix

  gameserver_cluster_arn = module.ecs.gameserver_cluster_arn

  tags = local.common_tags
}

# Control plane services: arena-api (behind the ALB, autoscaled) and
# arena-controller (desired_count=2, leader-elected, no LB).
module "control_plane" {
  source = "./modules/control-plane"

  name_prefix = local.name_prefix
  aws_region  = var.aws_region

  cluster_arn        = module.ecs.control_plane_cluster_arn
  cluster_name       = module.ecs.control_plane_cluster_name
  vpc_id             = module.vpc.vpc_id
  private_subnet_ids = module.vpc.private_subnet_ids
  alb_subnet_ids     = module.vpc.alb_subnet_ids

  alb_security_group_id        = module.vpc.alb_security_group_id
  api_security_group_id        = module.vpc.api_security_group_id
  controller_security_group_id = module.vpc.controller_security_group_id

  task_execution_role_arn  = module.iam.task_execution_role_arn
  api_task_role_arn        = module.iam.api_task_role_arn
  controller_task_role_arn = module.iam.controller_task_role_arn

  api_image         = var.api_image
  controller_image  = var.controller_image
  api_desired_count = var.api_desired_count
  api_max_count     = var.api_max_count
  certificate_arn   = var.certificate_arn

  dynamodb_table_names   = module.dynamodb.table_names
  redis_endpoint         = module.redis.primary_endpoint_address
  events_queue_url       = module.events.queue_url
  gameserver_cluster_arn = module.ecs.gameserver_cluster_arn

  tags = local.common_tags
}

# Alarms (DESIGN_v2 §7.3). Metrics arrive via EMF, not PutMetricData.
module "monitoring" {
  source = "./modules/monitoring"

  name_prefix = local.name_prefix

  events_queue_name   = module.events.queue_name
  sns_alarm_topic_arn = var.sns_alarm_topic_arn

  tags = local.common_tags
}

# Outputs
output "vpc_id" {
  value = module.vpc.vpc_id
}

output "control_plane_cluster_arn" {
  value = module.ecs.control_plane_cluster_arn
}

output "gameserver_cluster_arn" {
  value = module.ecs.gameserver_cluster_arn
}

output "dynamodb_tables" {
  value = module.dynamodb.table_names
}

output "redis_endpoint" {
  value     = module.redis.primary_endpoint_address
  sensitive = true
}

output "api_endpoint" {
  value = module.control_plane.api_endpoint
}

output "events_queue_url" {
  value = module.events.queue_url
}
