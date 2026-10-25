variable "name" {
  description = "Prefix for resource names, e.g. osp-prod."
  type        = string
}

variable "hostname" {
  description = "Public hostname, e.g. os.example.com."
  type        = string
}

variable "zone_id" {
  description = "Route 53 hosted zone that holds hostname."
  type        = string
}

variable "certificate_arn" {
  description = "Validated ACM certificate for hostname, in us-east-1."
  type        = string
}

variable "alb_arn" {
  type = string
}

variable "alb_dns_name" {
  type = string
}

variable "allowed_ip_cidrs" {
  description = "If set, only these IPv4 CIDRs may use the site (for a private staging environment)."
  type        = list(string)
  default     = []
}

variable "vm_create_limit_per_5min" {
  description = "POST /api/vms per client IP per 5 minutes (WAF minimum is 10)."
  type        = number
  default     = 30
}

variable "request_limit_per_5min" {
  description = "All requests per client IP per 5 minutes. Booting a VM alone polls about 600."
  type        = number
  default     = 3000
}

variable "force_destroy" {
  description = "Allow terraform destroy to delete a non-empty site bucket."
  type        = bool
  default     = false
}
