output "control_plane_cluster_arn" {
  value = aws_ecs_cluster.control_plane.arn
}

output "control_plane_cluster_name" {
  value = aws_ecs_cluster.control_plane.name
}

output "gameserver_cluster_arn" {
  value = aws_ecs_cluster.gameserver.arn
}

output "gameserver_cluster_name" {
  value = aws_ecs_cluster.gameserver.name
}
