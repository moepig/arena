locals {
  tables = {
    fleets      = aws_dynamodb_table.fleets
    gameservers = aws_dynamodb_table.gameservers
    allocations = aws_dynamodb_table.allocations
    leases      = aws_dynamodb_table.leases
  }
}

output "table_names" {
  value = { for k, t in local.tables : k => t.name }
}

output "table_arns" {
  value = { for k, t in local.tables : k => t.arn }
}
