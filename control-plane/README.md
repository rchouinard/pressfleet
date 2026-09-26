# Control plane

The Go control plane: API server, job workers, metrics collector, and the `pressfleet` CLI. The binary's entrypoint is [`cmd/pressfleet`](cmd/pressfleet/).

The control plane container image is built from [`Dockerfile`](Dockerfile) with `make image` (build context: the repository root). `make test-platform` builds it and brings up the platform services Compose project against the local Docker daemon, then removes everything it created.
