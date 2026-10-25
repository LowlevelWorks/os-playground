output "alb_arn" {
  value = aws_lb.this.arn
}

output "alb_dns_name" {
  value = aws_lb.this.dns_name
}

output "asg_name" {
  value = aws_autoscaling_group.api.name
}

output "asg_arn" {
  value = aws_autoscaling_group.api.arn
}

output "host_role_arn" {
  value = aws_iam_role.api.arn
}

output "image_parameter_name" {
  value = aws_ssm_parameter.image.name
}

output "image_parameter_arn" {
  value = aws_ssm_parameter.image.arn
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.api.name
}
