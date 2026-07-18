output "vpc_id" {
  value = aws_vpc.this.id
}

output "alb_subnet_ids" {
  value = aws_subnet.alb[*].id
}

output "gameserver_subnet_ids" {
  value = aws_subnet.gameserver[*].id
}

output "private_subnet_ids" {
  value = aws_subnet.private[*].id
}

output "alb_security_group_id" {
  value = aws_security_group.alb.id
}

output "api_security_group_id" {
  value = aws_security_group.api.id
}

output "controller_security_group_id" {
  value = aws_security_group.controller.id
}

output "gameserver_security_group_id" {
  value = aws_security_group.gameserver.id
}

output "redis_security_group_id" {
  value = aws_security_group.redis.id
}
