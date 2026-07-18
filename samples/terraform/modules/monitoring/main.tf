# The five alarms of DESIGN_v2 §7.3. Application metrics arrive via EMF
# structured logs (Arena/* namespaces), not PutMetricData.

locals {
  alarm_actions = var.sns_alarm_topic_arn == "" ? [] : [var.sns_alarm_topic_arn]
}

resource "aws_cloudwatch_metric_alarm" "high_allocation_latency" {
  alarm_name          = "${var.name_prefix}-high-allocation-latency"
  alarm_description   = "Allocation p99 latency above 500ms"
  namespace           = "Arena/Allocation"
  metric_name         = "AllocationLatency"
  extended_statistic  = "p99"
  period              = 60
  evaluation_periods  = 3
  threshold           = 500
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "high_allocation_error_rate" {
  alarm_name          = "${var.name_prefix}-high-allocation-error-rate"
  alarm_description   = "Allocation errors sustained"
  namespace           = "Arena/Allocation"
  metric_name         = "AllocationErrors"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 10
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "no_ready_gameservers" {
  alarm_name          = "${var.name_prefix}-no-ready-gameservers"
  alarm_description   = "Ready inventory exhausted (PoolMiss occurring)"
  namespace           = "Arena/Allocation"
  metric_name         = "PoolMiss"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  tags = var.tags
}

# Edge-trigger lag: controller falling behind on ECS task events.
resource "aws_cloudwatch_metric_alarm" "event_lag" {
  alarm_name          = "${var.name_prefix}-event-lag"
  alarm_description   = "ECS task events queue is backing up (> 30s old)"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 30
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    QueueName = var.events_queue_name
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "leader_lease_lost" {
  alarm_name          = "${var.name_prefix}-leader-lease-lost"
  alarm_description   = "No controller instance holds the leader lease"
  namespace           = "Arena/Controller"
  metric_name         = "LeaderLeaseHeld"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  tags = var.tags
}
