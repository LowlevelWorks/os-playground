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
HOST_GOOS   := $(shell go env GOOS)
TOOLS       := $(CURDIR)/.tools

# Keep GOLANGCI_LINT_VERSION in sync with `version:` of golangci-lint-action in ci.yml.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0
# The version is in the file name, so bumping it forces a reinstall.
GOLANGCI_LINT := $(TOOLS)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   := $(TOOLS)/govulncheck-$(GOVULNCHECK_VERSION)

.PHONY: help check-iso dev dev-api test test-integration lint lint-fix fmt tidy-check vuln build ci clean

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

lint: $(GOLANGCI_LINT) ## Run linters and format checks (golangci-lint)
	$(GOLANGCI_LINT) run
ifneq ($(HOST_GOOS),linux)
	@# Production runs on Linux: also lint the linux-only files this host skips.
	GOOS=linux $(GOLANGCI_LINT) run
endif

lint-fix: $(GOLANGCI_LINT) ## Run linters and apply safe auto-fixes
	$(GOLANGCI_LINT) run --fix

fmt: $(GOLANGCI_LINT) ## Format the code (gofmt + goimports)
	$(GOLANGCI_LINT) fmt

tidy-check: ## Fail if go.mod/go.sum are not tidy
	go mod tidy -diff

vuln: $(GOVULNCHECK) ## Scan for known vulnerabilities in reachable code
	$(GOVULNCHECK) ./...

build: ## Build the server binary into bin/
	go build -trimpath -o $(BIN) ./cmd/server

ci: tidy-check lint test build vuln ## Run everything CI runs

clean: ## Remove build output (keeps .tools)
	rm -rf bin

$(GOLANGCI_LINT):
	GOBIN=$(TOOLS) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(TOOLS)/golangci-lint $@

$(GOVULNCHECK):
	GOBIN=$(TOOLS) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(TOOLS)/govulncheck $@
