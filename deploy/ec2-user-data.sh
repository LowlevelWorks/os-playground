#!/bin/bash
set -euxo pipefail

REGION=ap-southeast-2
REGISTRY=496178998638.dkr.ecr.$REGION.amazonaws.com
IMAGE=$REGISTRY/os-playground-api:05ee3f2

dnf install -y docker
systemctl enable --now docker

mkdir -p /opt/os /etc/os-playground
aws s3 cp s3://os-playground-assets/os.iso /opt/os/os.iso --region $REGION

# Use KVM when the instance type provides it, otherwise software emulation
ACCEL=tcg
KVM_ARGS=()
if [ -e /dev/kvm ]; then
  ACCEL=kvm
  KVM_ARGS=(--device /dev/kvm --group-add "$(stat -c %g /dev/kvm)")
fi

cat > /etc/os-playground/api.env <<EOF
PLAYGROUND_QEMU_ACCEL=$ACCEL
PLAYGROUND_MAX_SESSIONS=4
PLAYGROUND_ALLOWED_ORIGINS=https://d3nih8cw5enbzj.cloudfront.net
PLAYGROUND_TRUSTED_PROXY_HOPS=2
EOF

aws ecr get-login-password --region $REGION \
  | docker login --username AWS --password-stdin $REGISTRY

docker run -d --name api --restart unless-stopped -p 8080:8080 \
  -v /opt/os/os.iso:/opt/os/os.iso:ro \
  --env-file /etc/os-playground/api.env \
  "${KVM_ARG
