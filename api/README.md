# API

The OpenAPI 3.1 spec (`openapi.yaml`) lives here and is the source of truth for the HTTP API. Go server code and the UI's TypeScript client are generated from it (design doc §4.5).

## Workflow

1. Edit `openapi.yaml`.
2. Run `make generate`. This regenerates `control-plane/internal/api/gen/api.gen.go`. Commit the result and never edit it by hand.
3. Implement or update the handler method in `control-plane/internal/api/` (one file per area). The router is generated, so adding an endpoint never means touching routing code.

CI runs `make generate` and fails if the generated code differs from what's committed.

The server loads the embedded spec at startup (`spec.go`). It validates every request against the spec, so handlers only receive requests that match the schema, and it uses each operation's `security` to decide which operations require authentication. Only login and the `/healthz` and `/readyz` probes are public. Authentication fails closed: if the server is built without an authenticator, every protected operation returns `401`. Handlers get the caller from `auth.PrincipalFromContext`; see the Authentication section of `openapi.yaml` for sessions, bearer tokens, and CSRF.

## Conventions

- All API endpoints live under `/api/v1`. The health probes (`/healthz`, `/readyz`) are the only exceptions.
- JSON field names are `snake_case`. Timestamps are RFC 3339 (`format: date-time`).
- Nullable fields use the 3.1 idiom `oneOf: [<schema>, {type: "null"}]` and generate Go pointers.
- Every error response is RFC 9457 Problem Details (`application/problem+json`, the `Problem` schema). Reuse the shared responses in `components/responses`.
- Long-running operations return `202 Accepted` with the queued `Job` in the body and a `Location` header pointing at `/api/v1/jobs/{job_id}`.
- List endpoints use cursor pagination: `limit` (1–200, default 50) and an opaque `cursor`. Responses carry `items` and `next_cursor` (null on the last page).
- Request bodies set `additionalProperties: false`, so unknown fields are rejected with `422`.
- Give every operation an `operationId`. It names the generated handler method.

## Generator choice

The generator is [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen) v2.8.0, pinned as a `tool` directive in `go.mod` and run with `go tool oapi-codegen`. Its configuration is in `control-plane/internal/api/gen/oapi-codegen.yaml`.

We evaluated these generators in September 2026:

| Generator | OpenAPI 3.1 | Server output | Notes |
| --- | --- | --- | --- |
| **oapi-codegen v2.8.0** (chosen) | Initial support since v2.8.0 (July 2026), built on kin-openapi: type arrays with `null`, `oneOf`-null nullability, `const` | Strict, typed server interface on standard-library `net/http` (Go 1.22+ `ServeMux` patterns) | Generated code depends only on the small `oapi-codegen/runtime` module. Widely used and actively maintained. Its 3.1 support is new, so unusual 3.1 constructs may not generate well. |
| ogen v1.24.0 | Partial (3.1 `null` types since v1.23.0) | Typed interface, but with its own router and JSON encoder (`go-faster/jx`) | Generated code pulls in OpenTelemetry, `go-faster/*`, and other dependencies. Doesn't use `net/http` routing, so our middleware and 404/405 handling would have to adapt to it. |
| Hand-written types plus `net/http` | n/a | n/a | No generator dependency, but the spec and the code would drift apart, which defeats spec-first (§4.5). |

oapi-codegen was chosen because it produces exactly what we need (a typed `StrictServerInterface` with standard-library routing) and keeps the runtime dependency footprint small. Its companion library, kin-openapi, also validates requests against the spec at runtime.

To upgrade it, run `go get -tool github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@<version>`, then `make generate`, and review the diff.
