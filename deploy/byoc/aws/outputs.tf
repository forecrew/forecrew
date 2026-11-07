output "agent_instance_id" {
  value       = aws_instance.agent.id
  description = "Look at it with forecrew provider joins, then confirm what it reports."
}

output "agent_kind" {
  value = local.agent_kind
}

output "machine_security_group_id" {
  value = local.is_ec2 ? aws_security_group.machines[0].id : null
}

output "agent_role_arn" {
  value = local.is_ec2 ? aws_iam_role.agent[0].arn : null
}

output "copied_ami_id" {
  value       = local.has_copy ? aws_ami_copy.image[0].id : null
  description = "Use ami:<region>/<this id> as the image's URI. It is usable once its state is available."
}
