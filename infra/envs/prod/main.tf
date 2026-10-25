terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.60"
    }
  }

  backend "s3" {
    bucket       = "osp-tfstate-496178998638"
    key          = "envs/prod/terraform.tfstate"
    region       = "ap-southeast-2"
    encrypt      = true
    use_lockfile = true
  }
}

locals {
  tags = {
    Project     = "os-playground"
    Environment = "prod"
    ManagedBy   = "terraform"
    Repository  = "LowlevelWorks/os-playground"
  }
}

provider "aws" {
  region = "ap-southeast-2"
  default_tags {
    tags = local.tags
  }
}

# CloudFront certificates and the CloudFront-scoped WAF live in us-east-1.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"
  default_tags {
    tags = local.tags
  }
}

data "terraform_remote_state" "global" {
  backend = "s3"
  config = {
    bucket = "osp-tfstate-496178998638"
    key    = "global/terraform.tfstate"
    region = "ap-southeast-2"
  }
}

module "environment" {
  source = "../../modules/environment"
  providers = {
    aws           = aws
    aws.us_east_1 = aws.us_east_1
  }

  environment = "prod"
  hostname    = var.hostname
  zone_id     = data.terraform_remote_state.global.outputs.zone_id

  vpc_cidr           = var.vpc_cidr
  az_count           = var.az_count
  single_nat_gateway = var.single_nat_gateway

  instance_type         = var.instance_type
  nested_virtualization = var.nested_virtualization
  min_size              = var.min_size
  max_size              = var.max_size
  desired_capacity      = var.desired_capacity
  max_sessions_per_host = var.max_sessions_per_host

  initial_image            = "${data.terraform_remote_state.global.outputs.ecr_repository_url}:${var.initial_image_tag}"
  ecr_repository_arn       = data.terraform_remote_state.global.outputs.ecr_repository_arn
  assets_bucket            = data.terraform_remote_state.global.outputs.assets_bucket
  github_oidc_provider_arn = data.terraform_remote_state.global.outputs.github_oidc_provider_arn
  github_repository        = data.terraform_remote_state.global.outputs.github_repository

  alarm_emails        = var.alarm_emails
  allowed_ip_cidrs    = var.allowed_ip_cidrs
  deletion_protection = var.deletion_protection
  log_retention_days  = var.log_retention_days
}

output "github_environment_variables" {
  description = "Copy into the GitHub environment \"prod\" as variables."
  value       = module.environment.github_environment_variables
}

output "api_log_group" {
  value = module.environment.api_log_group
}
