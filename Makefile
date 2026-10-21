VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
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

# The dashboard (ui/) is embedded into the binary when it has been built; see
# ui/README.md. UI selects what `make build` does:
#   auto (default)  build the dashboard if pnpm is on the PATH, else embed a
#                   placeholder page, so the Go build works without Node.js
#   yes             build the dashboard; fail if Node.js is missing (releases)
#   prebuilt        embed ui/.output/public as it is, without building it (CI,
#                   which builds the dashboard once in its own job)
#   no              embed the placeholder
UI ?= auto
UI_EMBED := $(if $(filter yes prebuilt,$(UI)),1,$(if $(filter auto,$(UI)),$(if $(shell command -v pnpm 2>/dev/null),1)))
UI_BUILD := $(if $(filter prebuilt,$(UI)),,$(UI_EMBED))
UI_DIST  := ui/.output/public/index.html
UI_SRC   := $(shell find ui/app ui/public -type f 2>/dev/null) ui/nuxt.config.ts ui/package.json ui/pnpm-lock.yaml
GO_TAGS  := $(if $(UI_EMBED),-tags webui)

.PHONY: build
build: $(if $(UI_BUILD),$(UI_DIST)) ## Build the pressfleet binary into bin/ (UI=yes|prebuilt|no|auto controls embedding the dashboard)
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath $(GO_TAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./control-plane/cmd/pressfleet

.PHONY: ui
ui: $(UI_DIST) ## Build the dashboard (needs Node.js and pnpm) into ui/.output/public

$(UI_DIST): $(UI_SRC) ui/node_modules/.modules.yaml
	pnpm -C ui generate
	@touch $@

ui/node_modules/.modules.yaml: ui/package.json ui/pnpm-lock.yaml
	pnpm -C ui install --frozen-lockfile

.PHONY: ui-client
ui-client: ui/node_modules/.modules.yaml ## Regenerate the TypeScript API client from api/openapi.yaml
	pnpm -C ui generate:api

.PHONY: ui-check
ui-check: ui/node_modules/.modules.yaml ## Lint, typecheck, and unit-test the dashboard
	pnpm -C ui lint
	pnpm -C ui typecheck
	pnpm -C ui test

.PHONY: test
test: ## Run tests with the race detector
	CGO_ENABLED=1 go test -race ./...

# Extra `docker build` options for builds behind a TLS-intercepting proxy,
# from PRESSFLEET_BUILD_EXTRA_CA, PRESSFLEET_BUILD_NETWORK, and the proxy
# variables (images/build-flags.sh; docs/testing.md). Empty when none is set.
# The two settings are passed on so that they also work as make arguments.
DOCKER_BUILD_FLAGS = $(shell PRESSFLEET_BUILD_EXTRA_CA='$(PRESSFLEET_BUILD_EXTRA_CA)' PRESSFLEET_BUILD_NETWORK='$(PRESSFLEET_BUILD_NETWORK)' images/build-flags.sh)

.PHONY: image
image: ## Build the control plane container image (tag: IMAGE=...)
	docker build $(DOCKER_BUILD_FLAGS) -f control-plane/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE) .

DEV_IMAGE ?= pressfleet-control-plane:dev

.PHONY: dev-up
dev-up: build ## Install and start a local dev platform in ./.dev (settings in .env, see CONTRIBUTING.md)
	$(MAKE) image IMAGE=$(DEV_IMAGE)
	PF_DEV_IMAGE=$(DEV_IMAGE) scripts/dev.sh up

.PHONY: dev-update
dev-update: build ## Rebuild the control plane image and recreate the dev platform's control plane on it
	$(MAKE) image IMAGE=$(DEV_IMAGE)
	PF_DEV_IMAGE=$(DEV_IMAGE) scripts/dev.sh update

.PHONY: dev-down
dev-down: ## Remove the local dev platform, its sites and data (PURGE_CERTS=1 also drops its certificates)
	PURGE_CERTS=$(PURGE_CERTS) scripts/dev.sh down

.PHONY: dev-status
dev-status: ## Show the local dev platform's containers
	scripts/dev.sh status

.PHONY: test-platform
test-platform: ## Bring up the platform project with a local image against real Docker, then tear it down
	$(MAKE) image IMAGE=pressfleet-control-plane:platform-test
	PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:platform-test \
		go test -count=1 -v -run TestPlatformUp ./control-plane/internal/platform/; \
		status=$$?; docker image rm pressfleet-control-plane:platform-test >/dev/null; exit $$status

.PHONY: test-e2e
test-e2e: ## Run the end-to-end tests against a freshly built image and real Docker (see docs/testing.md)
	$(MAKE) image IMAGE=pressfleet-control-plane:e2e
	GH_TOKEN="$${GH_TOKEN:-$$(gh auth token 2>/dev/null)}" \
	PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:e2e \
		go test -count=1 -v -timeout 60m ./control-plane/internal/e2e/; \
		status=$$?; docker image rm pressfleet-control-plane:e2e >/dev/null; exit $$status

# The Playwright suite in ui/e2e, against a platform booted by the e2e
# harness (control-plane/internal/e2e/dashboard). PLAYWRIGHT_ARGS go to
# `playwright test`, e.g. PLAYWRIGHT_ARGS="--headed -g provision".
.PHONY: test-ui-e2e
test-ui-e2e: ui/node_modules/.modules.yaml ## Run the dashboard's end-to-end tests (Playwright) against a freshly built image and real Docker
	$(MAKE) image IMAGE=pressfleet-control-plane:ui-e2e
	PRESSFLEET_DOCKER_TESTS=1 PRESSFLEET_TEST_CONTROL_PLANE_IMAGE=pressfleet-control-plane:ui-e2e \
	PRESSFLEET_UI_E2E_ARGS="$(PLAYWRIGHT_ARGS)" \
		go test -count=1 -v -timeout 40m ./control-plane/internal/e2e/dashboard/; \
		status=$$?; docker image rm pressfleet-control-plane:ui-e2e >/dev/null; exit $$status

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

# govulncheck reports the known vulnerabilities the code can reach. Pinned,
# like every tool CI runs; `go run` builds it with the mise Go toolchain, so
# it checks the standard library the binary is built with.
GOVULNCHECK_VERSION ?= v1.9.0

.PHONY: vulncheck
vulncheck: ## Check Go code and dependencies for known vulnerabilities (govulncheck)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: generate
generate: ## Run code generators (OpenAPI server and clients, TypeScript client; needs Node.js)
	go generate ./...
	$(MAKE) ui-client

.PHONY: fmt
fmt: ## Format Go code
	golangci-lint fmt ./...

.PHONY: tidy
tidy: ## Tidy go.mod/go.sum
	go mod tidy

IMAGE_REPO ?= $(shell images/matrix.sh registry)
# PHP branches `make images` builds; empty means every branch in the matrix.
SITE_PHP ?=

.PHONY: images
images: ## Build the site image matrix from images/manifest.json (needs jq; SITE_PHP="8.5 8.4" limits the PHP branches)
	@set -e; php="$(if $(SITE_PHP),$(SITE_PHP),$$(images/matrix.sh php))"; wps="$$(images/matrix.sh wp)"; \
	for wp in $$wps; do \
		sha1="$$(images/matrix.sh sha1 $$wp)"; \
		docker build $(DOCKER_BUILD_FLAGS) -f images/site-nginx/Dockerfile -t $(IMAGE_REPO)/site-nginx:wp$$wp \
			--build-arg WP_VERSION=$$wp --build-arg WP_SHA1=$$sha1 images; \
		for v in $$php; do \
			docker build $(DOCKER_BUILD_FLAGS) -f images/site-php/Dockerfile -t $(IMAGE_REPO)/site-php:php$$v-wp$$wp \
				--build-arg PHP_VERSION=$$v --build-arg WP_VERSION=$$wp --build-arg WP_SHA1=$$sha1 images; \
		done; \
	done

.PHONY: images-test
images-test: ## Build the site images and run their smoke test (needs Docker)
	images/test.sh

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out ui/.output ui/.nuxt
