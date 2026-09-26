VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/pressfleet
IMAGE   ?= ghcr.io/rchouinard/pressfleet/control-plane:$(VERSION)

# PressFleet only supports linux/amd64 hosts (design doc §3.1).
GOOS   ?= linux
GOARCH ?= amd64

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  %-14s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the pressfleet binary into bin/
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./control-plane/cmd/pressfleet

.PHONY: test
test: ## Run tests with the race detector
	CGO_ENABLED=1 go test -race ./...

.PHONY: image
image: ## Build the control plane container image (tag: IMAGE=...)
	docker build -f control-plane/Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: test-platform
test-platform: ## Bring up the platform project with a local image against real Docker, then tear it down
	$(MAKE) image IMAGE=pressfleet-control-plane:platform-test
	PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:platform-test \
		go test -count=1 -v -run TestPlatformUp ./control-plane/internal/platform/; \
		status=$$?; docker image rm pressfleet-control-plane:platform-test >/dev/null; exit $$status

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

IMAGE_REPO ?= ghcr.io/rchouinard/pressfleet

.PHONY: images
images: ## Build the site images (versions from images/versions.env)
	@set -e; . images/versions.env; \
	docker build -f images/site-nginx/Dockerfile -t $(IMAGE_REPO)/site-nginx:wp$$WP_VERSION \
		--build-arg WP_VERSION=$$WP_VERSION --build-arg WP_SHA1=$$WP_SHA1 images; \
	for v in $$PHP_VERSIONS; do \
		docker build -f images/site-php/Dockerfile -t $(IMAGE_REPO)/site-php:php$$v-wp$$WP_VERSION \
			--build-arg PHP_VERSION=$$v --build-arg WP_VERSION=$$WP_VERSION --build-arg WP_SHA1=$$WP_SHA1 images; \
	done

.PHONY: images-test
images-test: ## Build the site images and run their smoke test (needs Docker)
	images/test.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out
