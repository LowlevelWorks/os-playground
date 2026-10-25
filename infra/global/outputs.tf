output "zone_id" {
  value = aws_route53_zone.this.zone_id
}

output "zone_name" {
  value = aws_route53_zone.this.name
}

output "zone_name_servers" {
  description = "Add these as NS records for zone_name at the parent domain's DNS provider."
  value       = aws_route53_zone.this.name_servers
}

output "ecr_repository_arn" {
  value = aws_ecr_repository.api.arn
}

output "ecr_repository_url" {
  value = aws_ecr_repository.api.repository_url
}

output "assets_bucket" {
  value = aws_s3_bucket.assets.id
}

output "github_repository" {
  value = var.github_repository
}

output "github_oidc_provider_arn" {
  value = local.github_oidc_provider_arn
}

output "github_role_arns" {
  description = "Repository variables for the CI workflows (see infra/README.md)."
  value = {
    AWS_PLAN_ROLE_ARN  = aws_iam_role.github["plan"].arn
    AWS_APPLY_ROLE_ARN = aws_iam_role.github["apply"].arn
    AWS_BUILD_ROLE_ARN = aws_iam_role.github["build"].arn
  }
}
