variable "name" {
  description = "Prefix for resource names, e.g. osp-prod."
  type        = string
}

variable "cidr" {
  description = "VPC CIDR. Give each environment its own range so they can be peered later."
  type        = string
}

variable "az_count" {
  description = "Availability zones to spread subnets over."
  type        = number
  default     = 3

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 4
    error_message = "az_count must be between 2 and 4 (the internal ALB needs at least 2)."
  }
}

variable "single_nat_gateway" {
  description = "One NAT gateway for all AZs instead of one per AZ. Cheaper, but egress fails with that AZ."
  type        = bool
  default     = false
}

variable "log_retention_days" {
  description = "Retention for VPC flow logs."
  type        = number
  default     = 30
}
