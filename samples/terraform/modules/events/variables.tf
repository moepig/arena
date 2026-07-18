variable "name_prefix" {
  type = string
}

variable "gameserver_cluster_arn" {
  description = "Only task state changes from this cluster are forwarded"
  type        = string
}

variable "tags" {
  type    = map(string)
  default = {}
}
