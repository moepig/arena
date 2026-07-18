variable "name_prefix" {
  type = string
}

variable "events_queue_name" {
  type = string
}

variable "sns_alarm_topic_arn" {
  type    = string
  default = ""
}

variable "tags" {
  type    = map(string)
  default = {}
}
