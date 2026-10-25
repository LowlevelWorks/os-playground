variable "name" {
  description = "Prefix for resource names, e.g. osp-prod."
  type        = string
}

variable "vpc_id" {
  type = string
}

variable "private_subnet_ids" {
  type = list(string)
}

variable "egress_ready" {
  description = "Anything that must exist before hosts boot (the NAT routes); used only for ordering."
  type        = any
  default     = null
}

variable "certificate_arn" {
  description = "Regional ACM certificate for the public hostname. CloudFront forwards the viewer's Host header, so the ALB must present a certificate for it."
  type        = string
}

variable "allowed_origin" {
  description = "The site's origin, e.g. https://os.example.com. The API accepts terminal WebSockets only from it."
  type        = string
}

variable "initial_image" {
  description = "Image written to the release parameter on first apply. Deploys change it afterwards."
  type        = string
}

variable "ecr_repository_arn" {
  type = string
}

variable "assets_bucket" {
  description = "Bucket holding the OS image."
  type        = string
}

variable "iso_key" {
  description = "Key of the OS image in the assets bucket."
  type        = string
  default     = "os.iso"
}

variable "instance_type" {
  type    = string
  default = "c8i.large"
}

variable "nested_virtualization" {
  description = "Expose /dev/kvm to the host so QEMU runs guests with KVM. Needs an 8th-generation Intel type (c8i, m8i, r8i)."
  type        = bool
  default     = true
}

variable "ami_id" {
  description = "Pin an AMI instead of following the latest Amazon Linux 2023."
  type        = string
  default     = null
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

variable "vm_memory_mb" {
  type    = number
  default = 128
}

variable "max_sessions_per_host" {
  description = "VMs one host may run at once. Size it to the instance's memory and CPU."
  type        = number
  default     = 8
}

variable "max_vm_duration_seconds" {
  description = "Stickiness lasts as long as a VM can, so a browser keeps reaching the host that runs it."
  type        = number
  default     = 3600
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "log_retention_days" {
  type    = number
  default = 30
}

variable "alarm_topic_arn" {
  type = string
}
