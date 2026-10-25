# Production: three AZs, a NAT gateway per AZ, at least two hosts in
# different AZs, KVM-accelerated guests.

hostname = "os.example.com" # TODO: the zone apex from infra/global

vpc_cidr           = "10.20.0.0/16"
az_count           = 3
single_nat_gateway = false

instance_type         = "c8i.large"
nested_virtualization = true
min_size              = 2
max_size              = 6
desired_capacity      = 2
max_sessions_per_host = 8

initial_image_tag = "05ee3f2"

alarm_emails        = ["you@example.com"] # TODO
deletion_protection = true
log_retention_days  = 90
