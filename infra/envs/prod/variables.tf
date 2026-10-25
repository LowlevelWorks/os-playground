variable "hostname" {
  type = string
}

variable "vpc_cidr" {
  type = string
}

variable "az_count" {
  type = number
}

variable "single_nat_gateway" {
  type = bool
}

variable "instance_type" {
  type = string
}

variable "nested_virtualization" {
  type = bool
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
  type = number
}

variable "initial_image_tag" {
  description = "Tag in ECR for the very first apply. Deploys own the running image afterwards."
  type        = string
}

variable "alarm_emails" {
  type = list(string)
}

variable "allowed_ip_cidrs" {
  type    = list(string)
  default = []
}

variable "deletion_protection" {
  type = bool
}

variable "log_retention_days" {
  type = number
}
