variable "zone_name" {
  description = "DNS zone for the playground, e.g. os.example.com. Prod serves its apex; staging a subdomain."
  type        = string
}

variable "github_repository" {
  description = "owner/name of the repository whose workflows may assume the CI roles."
  type        = string
  default     = "LowlevelWorks/os-playground"
}

variable "manage_github_oidc_provider" {
  description = "Create the GitHub OIDC provider here. Leave false if an administrator provides it."
  type        = bool
  default     = false
}

variable "monthly_budget_usd" {
  type    = number
  default = 500
}

variable "budget_emails" {
  type = list(string)
}
