variable "name_prefix" {
  type = string
}

variable "enable_point_in_time_recovery" {
  type    = bool
  default = true
}

variable "tags" {
  type    = map(string)
  default = {}
}
