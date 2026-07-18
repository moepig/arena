output "api_endpoint" {
  value = "${var.certificate_arn == "" ? "http" : "https"}://${aws_lb.api.dns_name}"
}

output "alb_arn" {
  value = aws_lb.api.arn
}

output "gameserver_log_group_name" {
  value = aws_cloudwatch_log_group.gameserver.name
}
