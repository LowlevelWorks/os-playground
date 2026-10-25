# Web ACL for the distribution (CLOUDFRONT scope lives in us-east-1).
# Managed rule groups for common attacks, plus rate limits sized to the
# frontend: it polls twice a second while a VM boots.

locals {
  restrict_by_ip = length(var.allowed_ip_cidrs) > 0
}

resource "aws_wafv2_ip_set" "allowed" {
  count              = local.restrict_by_ip ? 1 : 0
  provider           = aws.us_east_1
  name               = "${var.name}-allowed"
  scope              = "CLOUDFRONT"
  ip_address_version = "IPV4"
  addresses          = var.allowed_ip_cidrs
}

resource "aws_wafv2_web_acl" "this" {
  provider = aws.us_east_1
  name     = var.name
  scope    = "CLOUDFRONT"

  # With an allow-list (staging), only those addresses get in at all.
  dynamic "default_action" {
    for_each = local.restrict_by_ip ? [] : [1]
    content {
      allow {}
    }
  }

  dynamic "default_action" {
    for_each = local.restrict_by_ip ? [1] : []
    content {
      block {}
    }
  }

  dynamic "rule" {
    for_each = local.restrict_by_ip ? [1] : []
    content {
      name     = "allow-listed-ips"
      priority = 0
      action {
        allow {}
      }
      statement {
        ip_set_reference_statement {
          arn = aws_wafv2_ip_set.allowed[0].arn
        }
      }
      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "${var.name}-allow-listed"
        sampled_requests_enabled   = true
      }
    }
  }

  rule {
    name     = "rate-limit-vm-create"
    priority = 10
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit                 = var.vm_create_limit_per_5min
        aggregate_key_type    = "IP"
        evaluation_window_sec = 300
        scope_down_statement {
          and_statement {
            statement {
              byte_match_statement {
                field_to_match {
                  method {}
                }
                positional_constraint = "EXACTLY"
                search_string         = "POST"
                text_transformation {
                  priority = 0
                  type     = "NONE"
                }
              }
            }
            statement {
              byte_match_statement {
                field_to_match {
                  uri_path {}
                }
                positional_constraint = "EXACTLY"
                search_string         = "/api/vms"
                text_transformation {
                  priority = 0
                  type     = "NONE"
                }
              }
            }
          }
        }
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name}-rate-vm-create"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "rate-limit-all"
    priority = 20
    action {
      block {}
    }
    statement {
      rate_based_statement {
        limit                 = var.request_limit_per_5min
        aggregate_key_type    = "IP"
        evaluation_window_sec = 300
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.name}-rate-all"
      sampled_requests_enabled   = true
    }
  }

  dynamic "rule" {
    for_each = {
      30 = "AWSManagedRulesAmazonIpReputationList"
      40 = "AWSManagedRulesKnownBadInputsRuleSet"
      50 = "AWSManagedRulesCommonRuleSet"
    }
    content {
      name     = rule.value
      priority = rule.key
      override_action {
        none {}
      }
      statement {
        managed_rule_group_statement {
          vendor_name = "AWS"
          name        = rule.value
        }
      }
      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "${var.name}-${rule.value}"
        sampled_requests_enabled   = true
      }
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = var.name
    sampled_requests_enabled   = true
  }
}
