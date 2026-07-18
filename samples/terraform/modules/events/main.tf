# Edge trigger (DESIGN_v2 §4.1.4):
#   ECS Task State Change → EventBridge rule → SQS → arena-controller
# The periodic resync (level trigger) catches anything dropped here.

resource "aws_sqs_queue" "dlq" {
  name = "${var.name_prefix}-ecs-events-dlq"

  message_retention_seconds = 1209600 # 14 days

  tags = var.tags
}

resource "aws_sqs_queue" "events" {
  name = "${var.name_prefix}-ecs-events"

  # Controller heartbeat-sweeps every 30s; keep visibility above the
  # consumer's processing deadline.
  visibility_timeout_seconds = 60
  message_retention_seconds  = 3600

  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 5
  })

  tags = var.tags
}

resource "aws_cloudwatch_event_rule" "task_state_change" {
  name        = "${var.name_prefix}-ecs-task-state-change"
  description = "GameServer cluster task state changes for arena-controller"

  event_pattern = jsonencode({
    source      = ["aws.ecs"]
    detail-type = ["ECS Task State Change"]
    detail = {
      clusterArn = [var.gameserver_cluster_arn]
    }
  })

  tags = var.tags
}

resource "aws_cloudwatch_event_target" "sqs" {
  rule = aws_cloudwatch_event_rule.task_state_change.name
  arn  = aws_sqs_queue.events.arn
}

data "aws_iam_policy_document" "queue_policy" {
  statement {
    effect    = "Allow"
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.events.arn]

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_cloudwatch_event_rule.task_state_change.arn]
    }
  }
}

resource "aws_sqs_queue_policy" "events" {
  queue_url = aws_sqs_queue.events.id
  policy    = data.aws_iam_policy_document.queue_policy.json
}
