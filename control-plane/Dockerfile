# syntax=docker/dockerfile:1
#
# Control plane image: ghcr.io/rchouinard/pressfleet/control-plane:<version>.
# Build from the repository root (the binary embeds ../templates):
#
#   docker build -f control-plane/Dockerfile --build-arg VERSION=v1.2.3 .
#
# or `make image`. Base images are pinned by linux/amd64 digest.

ARG GO_IMAGE=golang:1.27.1-trixie@sha256:7bffdb405cd12940d2980daa49a86ef575ed4525a17ee7d0c9562547357ab46a
# Source of the docker CLI and Compose v2 plugin. Both are static binaries.
ARG DOCKER_CLI_IMAGE=docker:29.8.1-cli@sha256:6602978e2be3c20e530e33773b8cadcef5fe998a71a534ee24516f1176973cdf
# Debian rather than distroless: restic and mariadb-dump join this image for
# backups later, and are easiest to install from Debian packages.
ARG BASE_IMAGE=debian:13.7-slim@sha256:7792b1f7702a86946cd518db72b6a407302c3e9bc1635634368b878189e8221c

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -buildvcs=false -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pressfleet ./control-plane/cmd/pressfleet

FROM ${DOCKER_CLI_IMAGE} AS docker-cli

FROM ${BASE_IMAGE}
# ca-certificates: TLS to registries, S3 backup targets, and ACME endpoints.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# The Compose driver shells out to `docker compose` (package
# internal/provisioner/compose), talking to the host daemon through the
# mounted /var/run/docker.sock.
COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
COPY --from=build /out/pressfleet /usr/local/bin/pressfleet

# The control plane runs as root in its container. It needs read-write access
# to the host's Docker socket, which is root-equivalent on the host anyway
# (design doc §6), so a non-root user would add no isolation. Root also keeps
# the root-owned, 0700 data directory and 0600 secret files readable without
# matching UIDs between host and container.
USER root
ENTRYPOINT ["pressfleet"]
CMD ["serve"]
