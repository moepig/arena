variable "name_prefix" {
  type = string
}

variable "vpc_cidr" {
  type = string
}

variable "availability_zones" {
  type = list(string)
}

variable "allowed_cidrs" {
  description = "CIDRs allowed to reach the control-plane ALB"
  type        = list(string)
}

variable "gameserver_port_range" {
  description = "Inbound port range for direct game traffic"
  type = object({
    from = number
    to   = number
  })
}

variable "tags" {
  type    = map(string)
  default = {}
}
