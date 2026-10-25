# One complete environment: network, API tier, edge, alerting, and the role
# its deploy job assumes. envs/staging and envs/prod differ only in inputs.

terraform {
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      configuration_aliases = [aws.us_east_1]
    }
  }
}

locals {
  name   = "osp-${var.environment}"
  origin = "https://${var.hostname}"
}

module "network" {
  source = "../network"

  name               = local.name
  cidr               = var.vpc_cidr
  az_count           = var.az_count
  single_nat_gateway = var.single_nat_gateway
  log_retention_days = var.log_retention_days
}

# ---- Certificates -----------------------------------------------------------------

# Two certificates for the same hostname: CloudFront only uses certificates in
# us-east-1, and the ALB needs one in its own region. The ALB's must be for the
# public hostname because CloudFront forwards the viewer's Host header and
# checks the origin's certificate against it.
resource "aws_acm_certificate" "site" {
  provider          = aws.us_east_1
  domain_name       = var.hostname
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_acm_certificate" "alb" {
  domain_name       = var.hostname
  validation_method = "DNS"

  lifecycle {
    create_before_destroy = true
  }
}

# ACM derives the validation record from the account and the domain, so both
# certificates ask for the same record. Keyed by domain (known at plan time,
# unlike the record name), it is created once and validates both.
resource "aws_route53_record" "cert_validation" {
  for_each = {
    for o in concat(
      tolist(aws_acm_certificate.site.domain_validation_options),
      tolist(aws_acm_certificate.alb.domain_validation_options),
    ) : o.domain_name => o...
  }
  zone_id         = var.zone_id
  name            = each.value[0].resource_record_name
  type            = each.value[0].resource_record_type
  records         = [each.value[0].resource_record_value]
  ttl             = 300
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "site" {
  provider                = aws.us_east_1
  certificate_arn         = aws_acm_certificate.site.arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

resource "aws_acm_certificate_validation" "alb" {
  certificate_arn         = aws_acm_certificate.alb.arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]
}

module "api" {
  source = "../api"

  name               = local.name
  vpc_id             = module.network.vpc_id
  private_subnet_ids = module.network.private_subnet_ids
  egress_ready       = module.network.private_routes_ready
  certificate_arn    = aws_acm_certificate_validation.alb.certificate_arn
  allowed_origin     = local.origin

  initial_image      = var.initial_image
  ecr_repository_arn = var.ecr_repository_arn
  assets_bucket      = var.assets_bucket
  iso_key            = var.iso_key

  instance_type         = var.instance_type
  nested_virtualization = var.nested_virtualization
  min_size              = var.min_size
  max_size              = var.max_size
  desired_capacity      = var.desired_capacity
  max_sessions_per_host = var.max_sessions_per_host

  deletion_protection = var.deletion_protection
  log_retention_days  = var.log_retention_days
  alarm_topic_arn     = aws_sns_topic.alarms.arn
}

module "edge" {
  source = "../edge"
  providers = {
    aws           = aws
    aws.us_east_1 = aws.us_east_1
  }

  name             = local.name
  hostname         = var.hostname
  zone_id          = var.zone_id
  certificate_arn  = aws_acm_certificate_validation.site.certificate_arn
  alb_arn          = module.api.alb_arn
  alb_dns_name     = module.api.alb_dns_name
  allowed_ip_cidrs = var.allowed_ip_cidrs
  force_destroy    = !var.deletion_protection
}

# ---- Alerting ---------------------------------------------------------------------

resource "aws_sns_topic" "alarms" {
  name = "${local.name}-alarms"
}

resource "aws_sns_topic_subscription" "alarm_email" {
  for_each  = toset(var.alarm_emails)
  topic_arn = aws_sns_topic.alarms.arn
  protocol  = "email"
  endpoint  = each.value
}

# ---- Deploy role (GitHub Actions, this environment only) -------------------------

resource "aws_iam_role" "deploy" {
  name = "${local.name}-deploy"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.github_oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "token.actions.githubusercontent.com:aud" = "sts.amazonaws.com"
          # Only jobs that run in this GitHub environment, which is limited
          # to the main branch.
          "token.actions.githubusercontent.com:sub" = "repo:${var.github_repository}:environment:${var.environment}"
        }
      }
    }]
  })
}

resource "aws_iam_role_policy" "deploy" {
  name = "deploy"
  role = aws_iam_role.deploy.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "SiteList"
        Effect   = "Allow"
        Action   = "s3:ListBucket"
        Resource = module.edge.site_bucket_arn
      },
      {
        Sid      = "SiteWrite"
        Effect   = "Allow"
        Action   = ["s3:PutObject", "s3:DeleteObject"]
        Resource = "${module.edge.site_bucket_arn}/*"
      },
      {
        Sid      = "Invalidate"
        Effect   = "Allow"
        Action   = "cloudfront:CreateInvalidation"
        Resource = module.edge.distribution_arn
      },
      {
        Sid      = "CheckImage"
        Effect   = "Allow"
        Action   = "ecr:DescribeImages"
        Resource = var.ecr_repository_arn
      },
      {
        Sid      = "Release"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:PutParameter"]
        Resource = module.api.image_parameter_arn
      },
      {
        Sid      = "Rollout"
        Effect   = "Allow"
        Action   = ["autoscaling:StartInstanceRefresh", "autoscaling:CancelInstanceRefresh"]
        Resource = module.api.asg_arn
      },
      {
        Sid      = "ReadRollout"
        Effect   = "Allow"
        Action   = ["autoscaling:DescribeInstanceRefreshes", "autoscaling:DescribeAutoScalingGroups"]
        Resource = "*"
      },
      {
        Sid       = "PassHostRole"
        Effect    = "Allow"
        Action    = "iam:PassRole"
        Resource  = module.api.host_role_arn
        Condition = { StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" } }
      },
    ]
  })
}
