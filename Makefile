VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/pressfleet

# PressFleet only supports linux/amd64 hosts (design doc §3.1).
GOOS   ?= linux
GOARCH ?= amd64

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-10s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the pressfleet binary into bin/
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./control-plane/cmd/pressfleet

.PHONY: test
test: ## Run tests with the race detector
	CGO_ENABLED=1 go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: generate
generate: ## Run code generators (OpenAPI, etc.)
	go generate ./...

.PHONY: fmt
fmt: ## Format Go code
	golangci-lint fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out
