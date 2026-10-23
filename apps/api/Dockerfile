# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.10

# ---- Stage 1: build the binary ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src

# Dependency manifests first, so a source change does not re-download modules.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd ./cmd
COPY internal ./internal

# Statically linked, cross-compiled from the build host to the target platform.
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 2: runtime ----
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends qemu-system-x86 \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10001 app \
 && useradd -u 10001 -g app -M -s /usr/sbin/nologin app

COPY --from=builder /out/server /usr/local/bin/server

# Listen on every interface; mount the OS image at PLAYGROUND_OS_IMAGE.
# QEMU defaults to tcg (software emulation). For kvm on a Linux host, run with
#   -e PLAYGROUND_QEMU_ACCEL=kvm --device /dev/kvm --group-add "$(stat -c %g /dev/kvm)"
# The server checks /dev/kvm at startup and refuses to start without it.
ENV PLAYGROUND_ADDR=:8080 \
    PLAYGROUND_OS_IMAGE=/opt/os/os.iso

USER app
EXPOSE 8080

# Exec form: no shell, and the binary probes the address it is configured with.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/server", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/server"]
