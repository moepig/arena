variable "aws_region" {
  description = "AWS region"
  type        = string
  default     = "us-west-2"
}

variable "environment" {
  description = "Environment name (dev, staging, prod)"
  type        = string
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.0.0.0/16"
}

variable "availability_zones" {
  description = "List of availability zones"
  type        = list(string)
  default     = ["us-west-2a", "us-west-2b", "us-west-2c"]
}

# DynamoDB
variable "enable_pitr" {
  description = "Enable Point-in-Time Recovery for DynamoDB"
  type        = bool
  default     = true
}

# Redis
variable "redis_node_type" {
  description = "ElastiCache node type"
  type        = string
  default     = "cache.r7g.large"
}

variable "redis_num_nodes" {
  description = "Number of cache nodes"
  type        = number
  default     = 2
}

# Control plane images (two services only: DESIGN_v2 §2.2)
variable "api_image" {
  description = "Docker image for arena-api (API + Allocation + SDK Gateway)"
  type        = string
}

variable "controller_image" {
  description = "Docker image for arena-controller (reconcilers + event consumer)"
  type        = string
}

# Control plane sizing
variable "api_desired_count" {
  description = "Initial desired count for arena-api (autoscaled 2..N)"
  type        = number
  default     = 2
}

variable "api_max_count" {
  description = "Autoscaling max for arena-api"
  type        = number
  default     = 10
}

# Networking
variable "allowed_cidrs" {
  description = "CIDRs allowed to reach the control-plane ALB"
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "certificate_arn" {
  description = "ACM certificate ARN for the ALB HTTPS listener. When empty, a plain HTTP listener is created (dev only)."
  type        = string
  default     = ""
}

variable "gameserver_port_range" {
  description = "Inbound UDP/TCP port range for game traffic (clients connect directly to task public IPs)"
  type = object({
    from = number
    to   = number
  })
  default = {
    from = 7000
    to   = 8000
  }
}

# Monitoring
variable "sns_alarm_topic_arn" {
  description = "SNS topic ARN for CloudWatch alarms"
  type        = string
  default     = ""
}
