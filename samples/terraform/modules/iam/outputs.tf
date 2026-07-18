output "task_execution_role_arn" {
  value = aws_iam_role.task_execution.arn
}

output "api_task_role_arn" {
  value = aws_iam_role.api.arn
}

output "controller_task_role_arn" {
  value = aws_iam_role.controller.arn
}

output "gameserver_task_role_arn" {
  value = aws_iam_role.gameserver.arn
}
