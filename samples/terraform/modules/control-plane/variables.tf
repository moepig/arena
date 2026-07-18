variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "cluster_arn" {
  type = string
}

variable "cluster_name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "private_subnet_ids" {
  type = list(string)
}

variable "alb_subnet_ids" {
  type = list(string)
}

variable "alb_security_group_id" {
  type = string
}

variable "api_security_group_id" {
  type = string
}

variable "controller_security_group_id" {
  type = string
}

variable "task_execution_role_arn" {
  type = string
}

variable "api_task_role_arn" {
  type = string
}

variable "controller_task_role_arn" {
  type = string
}

variable "api_image" {
  type = string
}

variable "controller_image" {
  type = string
}

variable "api_desired_count" {
  type    = number
  default = 2
}

variable "api_max_count" {
  type    = number
  default = 10
}

variable "certificate_arn" {
  description = "ACM certificate for HTTPS. Empty = HTTP listener (dev only)."
  type        = string
  default     = ""
}

variable "dynamodb_table_names" {
  type = map(string)
}

variable "redis_endpoint" {
  type = string
}

variable "events_queue_url" {
  type = string
}

variable "gameserver_cluster_arn" {
  type = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
