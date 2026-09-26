# PressFleet: working notes

Self-hostable WordPress application platform. The design lives in [docs/pressfleet_architecture_and_implementation_document.md](docs/pressfleet_architecture_and_implementation_document.md); the plan is in [docs/roadmap.md](docs/roadmap.md), with live status on the [project board](https://github.com/users/rchouinard/projects/3). Each GitHub issue has tasks and acceptance criteria. Read the issue (`gh issue view N`) and the design doc sections it references before starting.

## Tooling

Tool versions are pinned in `mise.toml`, and Go is not on the PATH without mise. In a fresh clone or git worktree:

```sh
mise trust && mise install
mise exec -- make test    # prefix Go/lint commands with `mise exec --`
```

| Command | Purpose |
| --- | --- |
| `make build` | Static linux/amd64 binary in `bin/pressfleet` |
| `make test` | `go test -race ./...` |
| `make lint` | golangci-lint (config in `.golangci.yml`) |
| `make generate` | Code generators |
| `make fmt` | Format |

CI (`.github/workflows/ci.yml`) runs lint, a `go mod tidy` check, tests, and the build. Keep all of them passing.

## Layout

- `control-plane/cmd/pressfleet`: entrypoint (thin; delegates to `internal/cli`)
- `control-plane/internal/<pkg>`: all Go code. Test fakes live in `<pkg>/<pkg>test`.
- `api/openapi.yaml`: API source of truth; generated code is never hand-edited
- `templates/`: Compose templates. `go:embed` can't reach outside a package directory, so templates are embedded through Go files in `templates/` itself (package `github.com/rchouinard/pressfleet/templates`)
- `images/<name>/`: container images (`site-php`, `site-nginx`)
- `ui/`: Nuxt dashboard (M5)

## Architecture rules

- All orchestration goes through `provisioner.Provisioner`. Docker, Compose, and Kubernetes SDK imports **and** invocations of the `docker` CLI belong only in `control-plane/internal/provisioner/compose/` (archtest enforces imports; review enforces the CLI rule). §4.2
- Site configuration is data (`provisioner.SiteConfig`), never Compose-specific fields.
- Everything that touches the state database must work on both SQLite and PostgreSQL, with tests on both.
- Job steps must be idempotent; permanently failed jobs roll back what they created. §5
- The host CLI never reads or writes the state database; state commands run inside the control plane container. §4.7
- Migrations are forward-only. §4.7
- Stay in MVP scope (§1.1). Don't build features that depend on open `type:decision` issues.

## Code conventions

- **Logging:** inject a `*slog.Logger`; never use the global `slog.Info` etc. (sloglint). Use static messages and key-value attributes.
- **Secrets:** hold them in `redact.Secret` and call `.Reveal()` only at the point of use. Plaintext secrets are never logged.
- **Errors:** wrap with `%w`; give each package sentinel errors for conditions callers branch on; compare with `errors.Is`/`errors.As`.
- **Config:** a new setting gets a `config.Config` field with a yaml tag, a `PRESSFLEET_*` env var, validation, and a row in [docs/configuration.md](docs/configuration.md).
- **Tests:** standard library `testing` only (no assertion libraries), table-driven where it fits. Unit tests must not need Docker. Tests that do need Docker are skipped unless `PRESSFLEET_DOCKER_TESTS=1`.
- **Dependencies:** prefer the standard library. Every new module needs a reason.
- **Doc comments** on exported identifiers; comments explain *why*, not *what*.

## Git

- Commit subject in the imperative, then a short bullet body. Reference issues with `Closes #N`.
- End commit messages with the `Co-Authored-By` trailer.
- Early-stage work is committed directly to `main`.
