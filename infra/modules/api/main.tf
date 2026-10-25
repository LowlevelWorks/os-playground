# The API tier: an internal ALB and an Auto Scaling group of EC2 hosts that
# each run the API container and the QEMU guests it starts.

data "aws_region" "current" {}
data "aws_caller_identity" "current" {}

# The latest Amazon Linux 2023 AMI. A new AMI shows up in the next plan as a
# launch template change, and applying it rolls the fleet onto the patched
# image through the group's instance refresh.
data "aws_ssm_parameter" "al2023" {
  name = "/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64"
}

data "aws_ec2_managed_prefix_list" "cloudfront" {
  name = "com.amazonaws.global.cloudfront.origin-facing"
}

locals {
  ami_id = coalesce(var.ami_id, nonsensitive(data.aws_ssm_parameter.al2023.value))
}

# ---- Release pointer --------------------------------------------------------

# Terraform creates it; CI owns its value from then on.
resource "aws_ssm_parameter" "image" {
  name        = "/${var.name}/api/image"
  description = "Image the API hosts run. Written by the deploy workflow."
  type        = "String"
  value       = var.initial_image

  lifecycle {
    ignore_changes = [value]
  }
}

resource "aws_cloudwatch_log_group" "api" {
  name              = "/${var.name}/api"
  retention_in_days = var.log_retention_days
}

# ---- Security groups ----------------------------------------------------------

resource "aws_security_group" "alb" {
  name        = "${var.name}-alb"
  description = "Internal ALB: HTTPS from the CloudFront VPC origin only"
  vpc_id      = var.vpc_id
  tags        = { Name = "${var.name}-alb" }
}

resource "aws_vpc_security_group_ingress_rule" "alb_https" {
  security_group_id = aws_security_group.alb.id
  description       = "CloudFront origin-facing (VPC origin)"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  prefix_list_id    = data.aws_ec2_managed_prefix_list.cloudfront.id
}

resource "aws_vpc_security_group_egress_rule" "alb_to_api" {
  security_group_id            = aws_security_group.alb.id
  description                  = "Forward to API hosts"
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
  referenced_security_group_id = aws_security_group.api.id
}

resource "aws_security_group" "api" {
  name        = "${var.name}-api"
  description = "API hosts: 8080 from the ALB only, HTTPS out"
  vpc_id      = var.vpc_id
  tags        = { Name = "${var.name}-api" }
}

resource "aws_vpc_security_group_ingress_rule" "api_from_alb" {
  security_group_id            = aws_security_group.api.id
  description                  = "From the ALB (traffic and health checks)"
  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
  referenced_security_group_id = aws_security_group.alb.id
}

# ECR, S3, SSM, CloudWatch Logs and the package repos are all HTTPS. DNS and
# the metadata service are not filtered by security groups.
resource "aws_vpc_security_group_egress_rule" "api_https" {
  security_group_id = aws_security_group.api.id
  description       = "HTTPS to AWS APIs and package repos"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"
}

# ---- Load balancer ----------------------------------------------------------

resource "aws_lb" "this" {
  name                       = "${var.name}-api"
  internal                   = true
  load_balancer_type         = "application"
  subnets                    = var.private_subnet_ids
  security_groups            = [aws_security_group.alb.id]
  drop_invalid_header_fields = true
  enable_deletion_protection = var.deletion_protection

  # A terminal WebSocket can sit quiet for a long time; the default of 60 s
  # would cut it off.
  idle_timeout = 3600

  access_logs {
    bucket  = aws_s3_bucket.alb_logs.id
    enabled = true
  }

  depends_on = [aws_s3_bucket_policy.alb_logs]
}

resource "aws_lb_target_group" "api" {
  name                 = "${var.name}-api"
  vpc_id               = var.vpc_id
  port                 = 8080
  protocol             = "HTTP"
  protocol_version     = "HTTP1"
  target_type          = "instance"
  deregistration_delay = 300

  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 10
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 2
  }

  # VMs live in one host's memory, so a browser must keep reaching the host
  # that created its VM. The ALB cookie passes through CloudFront because the
  # API behavior forwards all viewer cookies.
  stickiness {
    type            = "lb_cookie"
    enabled         = true
    cookie_duration = var.max_vm_duration_seconds
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}

resource "aws_s3_bucket" "alb_logs" {
  bucket        = "${var.name}-alb-logs-${data.aws_caller_identity.current.account_id}"
  force_destroy = !var.deletion_protection
}

resource "aws_s3_bucket_public_access_block" "alb_logs" {
  bucket                  = aws_s3_bucket.alb_logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256" # ALB log delivery supports SSE-S3 only
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id
  rule {
    id     = "expire"
    status = "Enabled"
    filter {}
    expiration {
      days = var.log_retention_days
    }
  }
}

data "aws_elb_service_account" "this" {}

resource "aws_s3_bucket_policy" "alb_logs" {
  bucket = aws_s3_bucket.alb_logs.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AlbLogDelivery"
        Effect    = "Allow"
        Principal = { AWS = data.aws_elb_service_account.this.arn }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.alb_logs.arn}/AWSLogs/${data.aws_caller_identity.current.account_id}/*"
      },
      {
        Sid       = "DenyInsecureTransport"
        Effect    = "Deny"
        Principal = "*"
        Action    = "s3:*"
        Resource  = [aws_s3_bucket.alb_logs.arn, "${aws_s3_bucket.alb_logs.arn}/*"]
        Condition = { Bool = { "aws:SecureTransport" = "false" } }
      },
    ]
  })
}

# ---- Hosts --------------------------------------------------------------------

resource "aws_launch_template" "api" {
  name                   = "${var.name}-api"
  description            = "API hosts for ${var.name}"
  image_id               = local.ami_id
  instance_type          = var.instance_type
  vpc_security_group_ids = [aws_security_group.api.id]
  update_default_version = true

  user_data = base64encode(templatefile("${path.module}/user-data.sh.tftpl", {
    region          = data.aws_region.current.region
    image_parameter = aws_ssm_parameter.image.name
    assets_bucket   = var.assets_bucket
    iso_key         = var.iso_key
    log_group       = aws_cloudwatch_log_group.api.name
    allowed_origin  = var.allowed_origin
    vm_memory_mb    = var.vm_memory_mb
    max_sessions    = var.max_sessions_per_host
  }))

  iam_instance_profile {
    arn = aws_iam_instance_profile.api.arn
  }

  dynamic "cpu_options" {
    for_each = var.nested_virtualization ? [1] : []
    content {
      nested_virtualization = "enabled"
    }
  }

  block_device_mappings {
    device_name = "/dev/xvda" # the root volume on Amazon Linux 2023
    ebs {
      volume_size           = 20
      volume_type           = "gp3"
      encrypted             = true
      delete_on_termination = true
    }
  }

  # IMDSv2 only, and a hop limit of 1 so containers (one hop further away on
  # Docker's bridge) cannot fetch the instance role's credentials.
  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  monitoring {
    enabled = true
  }

  tag_specifications {
    resource_type = "instance"
    tags          = { Name = "${var.name}-api" }
  }

  tag_specifications {
    resource_type = "volume"
    tags          = { Name = "${var.name}-api" }
  }
}

resource "aws_autoscaling_group" "api" {
  name                      = "${var.name}-api"
  vpc_zone_identifier       = var.private_subnet_ids
  min_size                  = var.min_size
  max_size                  = var.max_size
  desired_capacity          = var.desired_capacity
  health_check_type         = "ELB"
  health_check_grace_period = 300
  target_group_arns         = [aws_lb_target_group.api.arn]
  default_instance_warmup   = 300
  wait_for_capacity_timeout = "15m"

  # Pinning the number (not $Latest) makes every launch template change a
  # visible diff, which the instance_refresh block below then rolls out.
  launch_template {
    id      = aws_launch_template.api.id
    version = aws_launch_template.api.latest_version
  }

  # Replacements start before the old host goes away.
  instance_maintenance_policy {
    min_healthy_percentage = 100
    max_healthy_percentage = 200
  }

  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 100
      max_healthy_percentage = 200
      instance_warmup        = 300
      skip_matching          = true
    }
  }

  enabled_metrics = [
    "GroupDesiredCapacity",
    "GroupInServiceInstances",
    "GroupPendingInstances",
    "GroupTerminatingInstances",
    "GroupTotalInstances",
  ]

  tag {
    key                 = "Name"
    value               = "${var.name}-api"
    propagate_at_launch = false # the launch template tags instances
  }

  depends_on = [var.egress_ready]
}
