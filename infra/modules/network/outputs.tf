output "vpc_id" {
  value = aws_vpc.this.id
}

output "public_subnet_ids" {
  value = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  value = aws_subnet.private[*].id
}

# The routes must exist before anything in the private subnets needs egress.
output "private_routes_ready" {
  value = aws_route.private_nat[*].id
}
