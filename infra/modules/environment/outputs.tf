# Everything the deploy workflow needs. Copy these into the GitHub
# environment's variables (see infra/README.md).

output "github_environment_variables" {
  value = {
    AWS_DEPLOY_ROLE_ARN = aws_iam_role.deploy.arn
    SITE_URL            = local.origin
    WEB_BUCKET          = module.edge.site_bucket
    DISTRIBUTION_ID     = module.edge.distribution_id
    ASG_NAME            = module.api.asg_name
    IMAGE_PARAMETER     = module.api.image_parameter_name
  }
}

output "api_log_group" {
  value = module.api.log_group_name
}

output "alarm_topic_arn" {
  value = aws_sns_topic.alarms.arn
}
