# Least-privilege task roles (DESIGN_v2 §6.3):
#   api        — DynamoDB (4 tables) R/W
#   controller — api permissions + ECS task management + scoped PassRole + SQS
#   gameserver — CloudWatch Logs only. No DynamoDB / Redis / ECS access:
#                the attack surface must not extend into the data plane.

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

locals {
  table_arns = values(var.dynamodb_table_arns)
  # GSIs (namespace-name-index, fleet-index, session-index, gameserver-index)
  index_arns = [for arn in local.table_arns : "${arn}/index/*"]
}

# --- Shared execution role (image pull + log driver) -------------------------

resource "aws_iam_role" "task_execution" {
  name               = "${var.name_prefix}-task-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "task_execution" {
  role       = aws_iam_role.task_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# --- arena-api ---------------------------------------------------------------

resource "aws_iam_role" "api" {
  name               = "${var.name_prefix}-api"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = var.tags
}

data "aws_iam_policy_document" "dynamodb_rw" {
  statement {
    sid = "DynamoDBReadWrite"
    actions = [
      "dynamodb:GetItem",
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
      "dynamodb:Query",
      "dynamodb:BatchGetItem",
      "dynamodb:BatchWriteItem",
      "dynamodb:ConditionCheckItem",
      "dynamodb:TransactGetItems",
      "dynamodb:TransactWriteItems",
    ]
    resources = concat(local.table_arns, local.index_arns)
  }
}

resource "aws_iam_role_policy" "api_dynamodb" {
  name   = "dynamodb"
  role   = aws_iam_role.api.id
  policy = data.aws_iam_policy_document.dynamodb_rw.json
}

# Sidecar authentication: the gateway verifies presented task ARNs against
# DescribeTasks startedBy (DESIGN_v2 §6.4.3).
data "aws_iam_policy_document" "api_ecs_read" {
  statement {
    sid       = "DescribeGameServerTasks"
    actions   = ["ecs:DescribeTasks"]
    resources = ["*"]

    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [var.gameserver_cluster_arn]
    }
  }
}

resource "aws_iam_role_policy" "api_ecs_read" {
  name   = "ecs-read"
  role   = aws_iam_role.api.id
  policy = data.aws_iam_policy_document.api_ecs_read.json
}

# --- arena-controller --------------------------------------------------------

resource "aws_iam_role" "controller" {
  name               = "${var.name_prefix}-controller"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = var.tags
}

resource "aws_iam_role_policy" "controller_dynamodb" {
  name   = "dynamodb"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.dynamodb_rw.json
}

data "aws_iam_policy_document" "controller_ecs" {
  statement {
    sid = "TaskLifecycle"
    actions = [
      "ecs:RunTask",
      "ecs:StopTask",
      "ecs:DescribeTasks",
      "ecs:ListTasks",
    ]
    resources = ["*"]

    condition {
      test     = "ArnEquals"
      variable = "ecs:cluster"
      values   = [var.gameserver_cluster_arn]
    }
  }

  # RegisterTaskDefinition has no resource-level scoping.
  statement {
    sid = "TaskDefinitions"
    actions = [
      "ecs:RegisterTaskDefinition",
      "ecs:DescribeTaskDefinition",
    ]
    resources = ["*"]
  }

  # PassRole restricted to the game server roles only (DESIGN_v2 §6.3).
  statement {
    sid     = "PassGameServerRoles"
    actions = ["iam:PassRole"]
    resources = [
      aws_iam_role.gameserver.arn,
      aws_iam_role.task_execution.arn,
    ]

    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role_policy" "controller_ecs" {
  name   = "ecs"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.controller_ecs.json
}

data "aws_iam_policy_document" "controller_sqs" {
  statement {
    sid = "ConsumeTaskEvents"
    actions = [
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:GetQueueAttributes",
    ]
    resources = [var.events_queue_arn]
  }
}

resource "aws_iam_role_policy" "controller_sqs" {
  name   = "sqs"
  role   = aws_iam_role.controller.id
  policy = data.aws_iam_policy_document.controller_sqs.json
}

# --- GameServer tasks ---------------------------------------------------------

resource "aws_iam_role" "gameserver" {
  name               = "${var.name_prefix}-gameserver"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = var.tags
}

data "aws_iam_policy_document" "gameserver_logs" {
  statement {
    sid = "LogsOnly"
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]
    resources = ["arn:aws:logs:*:*:log-group:/${var.name_prefix}/gameserver*"]
  }
}

resource "aws_iam_role_policy" "gameserver_logs" {
  name   = "logs"
  role   = aws_iam_role.gameserver.id
  policy = data.aws_iam_policy_document.gameserver_logs.json
}
