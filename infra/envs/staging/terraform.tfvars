# Staging: same shape as prod, smaller. Every release runs here first.

hostname = "staging.os.example.com" # TODO: a subdomain of the zone from infra/global

vpc_cidr           = "10.10.0.0/16"
az_count           = 2
single_nat_gateway = true

instance_type         = "c8i.large"
nested_virtualization = true
min_size              = 1
max_size              = 2
desired_capacity      = 1
max_sessions_per_host = 8

initial_image_tag = "05ee3f2"

alarm_emails = ["you@example.com"] # TODO

# Keep staging private: your office/home IPs, plus nothing else. Empty = public.
allowed_ip_cidrs = []

deletion_protection = false
log_retention_days  = 14
