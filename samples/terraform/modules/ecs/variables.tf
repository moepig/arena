variable "name_prefix" {
  type = string
}

variable "enable_container_insights" {
  type    = bool
  default = true
}

variable "tags" {
  type    = map(string)
  default = {}
}
