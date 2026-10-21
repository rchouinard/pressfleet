# syntax=docker/dockerfile:1
#
# Control plane image: ghcr.io/rchouinard/pressfleet/control-plane:<version>.
# Build from the repository root (the binary embeds ../templates and the
# dashboard built from ../ui):
#
#   docker build -f control-plane/Dockerfile --build-arg VERSION=v1.2.3 .
#
# or `make image`. Base images are pinned by linux/amd64 digest.

ARG GO_IMAGE=golang:1.27.3-trixie@sha256:8aa2821993207ae840ce09e4223217efcd14e01f71525db734faf4232bd59999
# Source of the docker CLI and Compose v2 plugin. Both are static binaries.
ARG DOCKER_CLI_IMAGE=docker:29.9.2-cli@sha256:46d89b565041ec4d08f8a7657b4f4b08c8425a87896249f7ae1a10514235b7e6
# Builds the dashboard (ui/), which the binary embeds. Only used at build time.
ARG NODE_IMAGE=node:24-slim@sha256:a8efdaa8f2458a83ad61c9b8e957c94c2312a9e60604d5b9b093cf89ad9f5984
# Debian rather than distroless: the MariaDB client for backups comes from
# Debian packages.
ARG BASE_IMAGE=debian:13.7-slim@sha256:4876fdea29cac1a33a3195f1eb1952feb1fca7dd6dd00be3e2f1879cf20d34d2
# Backup tools (design doc §5.3). Backup tasks run in this image
# (provisioner.Task). restic is the upstream static binary, verified against
# the release's SHA256SUMS; the MariaDB client is the Debian trixie package.
ARG RESTIC_VERSION=0.19.1
ARG RESTIC_SHA256=f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c
ARG MARIADB_CLIENT_VERSION=1:11.8.6-0+deb13u1

# Every RUN that may download sources images/shared/extra-ca.sh first, which
# trusts the optional extra_ca build secret for that RUN only (builds behind
# a TLS-intercepting proxy; docs/testing.md). Without the secret it does
# nothing, and the CA never reaches an image layer.

FROM ${NODE_IMAGE} AS ui
# Keep in step with the pnpm version in mise.toml and ui/package.json.
RUN --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh && npm install --global pnpm@10.34.6
WORKDIR /src/ui
COPY ui/package.json ui/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh && pnpm install --frozen-lockfile --ignore-scripts
COPY ui/ ./
RUN --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh && pnpm generate

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh && go mod download
COPY . .
COPY --from=ui /src/ui/.output/public ui/.output/public
ARG VERSION=dev
# The commit the image is built from. Shown in the dashboard; the build
# itself runs with -buildvcs=false, so Go doesn't stamp it.
ARG COMMIT=
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -buildvcs=false -tags webui -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/pressfleet ./control-plane/cmd/pressfleet

FROM ${DOCKER_CLI_IMAGE} AS docker-cli

# mariadb-dump is in mariadb-client, which pulls in Perl for scripts the
# control plane never runs; only the binary is copied out of this stage.
FROM ${BASE_IMAGE} AS backup-tools
ARG RESTIC_VERSION
ARG RESTIC_SHA256
ARG MARIADB_CLIENT_VERSION
RUN --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh \
    && apt-get update \
    && apt-get install -y --no-install-recommends bzip2 "mariadb-client=${MARIADB_CLIENT_VERSION}" \
    && rm -rf /var/lib/apt/lists/*
ADD --checksum=sha256:${RESTIC_SHA256} \
    https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2 \
    /tmp/restic.bz2
RUN bunzip2 -c /tmp/restic.bz2 > /usr/local/bin/restic && chmod 0755 /usr/local/bin/restic

FROM ${BASE_IMAGE}
ARG MARIADB_CLIENT_VERSION
# ca-certificates: TLS to registries, S3 backup targets, and ACME endpoints.
# mariadb-client-core: the `mariadb` client and the libraries mariadb-dump
# links against, without mariadb-client's Perl dependencies.
RUN --mount=type=secret,id=extra_ca,required=false \
    --mount=type=bind,source=images/shared/extra-ca.sh,target=/run/extra-ca.sh \
    . /run/extra-ca.sh \
    && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates "mariadb-client-core=${MARIADB_CLIENT_VERSION}" \
    && rm -rf /var/lib/apt/lists/*
COPY --from=backup-tools /usr/bin/mariadb-dump /usr/bin/mariadb-dump
COPY --from=backup-tools /usr/local/bin/restic /usr/local/bin/restic
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
# The host CLI reads the version from this label when the control plane
# doesn't answer, to enforce the version skew policy (design doc §4.7).
ARG VERSION=dev
LABEL org.opencontainers.image.version=${VERSION}
ENTRYPOINT ["pressfleet"]
CMD ["serve"]
