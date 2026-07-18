variable "name_prefix" {
  type = string
}

variable "dynamodb_table_arns" {
  type = map(string)
}

variable "events_queue_arn" {
  type = string
}

variable "gameserver_cluster_arn" {
  type = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
