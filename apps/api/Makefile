# API Makefile. Run from apps/api, or from anywhere with: make -C apps/api <target>

SHELL       := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

# The OS image is built outside this repo. Override on the command line:
#   make dev ISO=/path/to/os.iso
ISO         ?= ../../../my-os/build/os.iso
ISO_ABS     := $(abspath $(ISO))
BOOT_MARKER ?= x86_64 Kernel
BIN         := bin/server

.PHONY: help check-iso dev dev-api test test-integration vet fmt fmt-check lint build clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

check-iso:
	@test -f "$(ISO_ABS)" || { \
		echo "OS image not found: $(ISO_ABS)"; \
		echo "Build the OS first, or point at an image: make $(MAKECMDGOALS) ISO=/path/to/os.iso"; \
		exit 1; \
	}

dev: dev-api ## Alias for dev-api

dev-api: check-iso ## Run the API against the OS image
	PLAYGROUND_OS_IMAGE="$(ISO_ABS)" go run ./cmd/server

test: ## Unit tests with the race detector
	go test -race -count=1 -shuffle=on ./...

test-integration: check-iso ## Boot the real OS in QEMU and check the console
	PLAYGROUND_OS_IMAGE="$(ISO_ABS)" \
	PLAYGROUND_BOOT_MARKER='$(BOOT_MARKER)' \
	go test -tags integration -race -count=1 -v ./internal/vm/localqemu/ -run 'TestRealQEMU'

vet: ## go vet
	go vet ./...

fmt: ## Format the code with gofmt
	gofmt -w .

fmt-check: ## Fail if any file needs gofmt
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "These files need gofmt (run: make fmt):"; echo "$$out"; exit 1; fi

lint: vet fmt-check ## vet + fmt-check

build: ## Build the server binary into bin/
	go build -trimpath -o $(BIN) ./cmd/server

clean: ## Remove build output
	rm -rf bin
