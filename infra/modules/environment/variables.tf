variable "environment" {
  description = "staging or prod. Also the GitHub environment whose jobs may deploy here."
  type        = string

  validation {
    condition     = contains(["staging", "prod"], var.environment)
    error_message = "environment must be staging or prod."
  }
}

variable "hostname" {
  description = "Public hostname, e.g. os.example.com."
  type        = string
}

variable "zone_id" {
  description = "Route 53 hosted zone for hostname (from the global stack)."
  type        = string
}

# ---- Network ----

variable "vpc_cidr" {
  type = string
}

variable "az_count" {
  type    = number
  default = 3
}

variable "single_nat_gateway" {
  type    = bool
  default = false
}

# ---- API hosts ----

variable "instance_type" {
  type    = string
  default = "c8i.large"
}

variable "nested_virtualization" {
  type    = bool
  default = true
}

variable "min_size" {
  type = number
}

variable "max_size" {
  type = number
}

variable "desired_capacity" {
  type = number
}

variable "max_sessions_per_host" {
  type    = number
  default = 8
}

# ---- Shared resources (from the global stack) ----

variable "initial_image" {
  description = "Image for the first apply; deploys own it afterwards."
  type        = string
}

variable "ecr_repository_arn" {
  type = string
}

variable "assets_bucket" {
  type = string
}

variable "iso_key" {
  type    = string
  default = "os.iso"
}

variable "github_oidc_provider_arn" {
  type = string
}

variable "github_repository" {
  description = "owner/name of the repository whose workflows deploy here."
  type        = string
}

# ---- Operations ----

variable "alarm_emails" {
  description = "Addresses subscribed to alarms. Each must confirm the subscription email."
  type        = list(string)
  default     = []
}

variable "allowed_ip_cidrs" {
  description = "If set, only these IPv4 CIDRs reach the site (keeps staging private)."
  type        = list(string)
  default     = []
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "log_retention_days" {
  type    = number
  default = 30
}
