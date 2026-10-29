# MCP Server: Implementation Plan

**Status:** Proposed, 2026-10-28. The decisions below wait on [#232](https://github.com/rchouinard/pressfleet/issues/232). Milestone: [Post-MVP: MCP Server](https://github.com/rchouinard/pressfleet/milestone/9); tracking issue: [#132](https://github.com/rchouinard/pressfleet/issues/132).

This plan adds a Model Context Protocol server to the control plane. It gives agentic clients the dashboard's operations: Claude Code, Claude Desktop, IDE agents, and CI agents. An agent can, for example, create a site, back it up, read its metrics and logs, and follow the jobs it started. The server goes through the same authorization check and audit log as the HTTP API (design doc §1.1, §8), so it adds a way in, not a new set of permissions.

## What the MVP already provides

- **A spec-first API.** `api/openapi.yaml` describes 128 operations, and the Go server is generated from it. The spec's JSON Schemas are OpenAPI 3.1, which is JSON Schema 2020-12, the dialect MCP tools use.
- **One authorization check per operation.** `operationActions` declares each operation's action, and the API refuses to start if one is missing. A handler that checks a different action is refused.
- **Token scopes.** A token's permissions are the intersection of its scope (`full` or `read-only`) and its owner's current role (§4.6). Some actions are session-only.
- **One handler chain.** Cross-origin checks, authentication, audit, request validation, and JSON decoding all sit in one `http.Handler` (package `api`). Errors are RFC 9457 Problem Details with field-level detail.
- **An audit log with free-form details.** Audit events have a `Details` map, which can say a call came through MCP without a migration.
- **Jobs.** Long operations return `202` with a job. `streamJobEvents` streams the job's steps, and `listSiteLogs` is a bounded log query.

These pieces are missing:

- **The MCP endpoint, sessions, and protocol handling.**
- **A way to turn operations into tools**, and to keep the tools in step with the spec.
- **Marks on secret fields.** Nothing in the spec says which fields hold secrets, so nothing can check that a tool never returns one.
- **A way to tell an MCP call from an HTTP call** in the audit log.

## Decisions

Proposed. Each one is confirmed or changed in [#232](https://github.com/rchouinard/pressfleet/issues/232), which also answers #132's open questions and lists the alternatives.

| Topic | Proposal |
| --- | --- |
| Authentication | Existing bearer API tokens, with cookies ignored. OAuth 2.1 sign-in is a P2 follow-up ([#241](https://github.com/rchouinard/pressfleet/issues/241)). |
| Secrets | No tool returns a secret or takes one as input. Secret fields in the spec are marked with `x-pressfleet-secret`, and CI checks the tools against the marks. |
| Destructive operations | Reads, writes, and destructive writes are annotated. Irreversible deletions and privilege changes are left out, and can come back behind MCP elicitation ([#242](https://github.com/rchouinard/pressfleet/issues/242), P2). |
| Uploads | Out of scope. Agents use the HTTP API for imports and deploys. |
| Local clients | Streamable HTTP only; no stdio bridge. |
| Library | The official Go SDK, `github.com/modelcontextprotocol/go-sdk`. |
| Tool shape | One tool per included operation, generated from the spec and an overlay file. Inputs are the parameters plus `body`; outputs are the response schema. |
| Jobs | Tools return the job, and `wait_for_job` follows it with progress notifications. |
| Endpoint | `/mcp` on the dashboard domain, off by default. It validates `Origin` and keeps sessions in memory. Every call is dispatched through the API handler in-process. |
| Audit and untrusted content | Audit details record `via: mcp` and the client's name. Results are JSON, and the descriptions flag site-controlled text. |

## Scope

**In scope:**

- the tool catalog and its generator
- the Streamable HTTP endpoint and its configuration
- tool calls through the API handler
- waiting for jobs
- MCP metrics
- a dashboard section for connecting clients
- end-to-end tests, a security review, and docs

**In the milestone at P2, not in its done-when:** OAuth 2.1 sign-in ([#241](https://github.com/rchouinard/pressfleet/issues/241)), and irreversible operations behind elicitation ([#242](https://github.com/rchouinard/pressfleet/issues/242)).

**Out of scope, for later:**

- MCP resources and prompts
- MCP tasks for jobs, once clients support them
- uploads and downloads through MCP
- a stdio bridge
- per-token rate limits, unless the security review asks for them ([#239](https://github.com/rchouinard/pressfleet/issues/239))

## How a tool call flows

```
MCP client ──HTTPS──▶ Traefik ──▶ control plane
                                    /mcp   (package mcp, Go SDK: Streamable HTTP, sessions,
                                            Origin check, bearer token via api.Authenticator)
                                      │ tools/list: catalog filtered by authz.Check(principal, action, platform)
                                      │ tools/call: catalog entry → HTTP request
                                      ▼
                                    api.Handler (in-process, same as /api/v1)
                                      cross-origin → authentication → audit (+ via: mcp) → validation → handler
                                      │                                                       │
                                      │                                         authz.Check(principal, action, target)
                                      ▼                                                       ▼
                                    response → structuredContent + text        services (sites, backups, jobs, …)
                                    Problem Details → isError result
```

The MCP layer only translates. Authentication, authorization, validation, audit, and errors are the HTTP API's. A tool can therefore never do something the matching HTTP request couldn't, and a fix to the API applies to both.

## Design

### The tool catalog

[#233](https://github.com/rchouinard/pressfleet/issues/233) builds the catalog from two sources:

- **`api/openapi.yaml`** stays the source of truth for operations, parameters, schemas, and authorization actions.
- **`api/mcp-tools.yaml`**, an overlay with an entry for every operationId. An entry either includes the operation, with a tool name, title, model-facing description, and annotations, or excludes it, with a reason.

`make generate` builds a Go table from both. Each entry has:

- the tool name
- the operation it calls
- its authorization actions
- an input schema: path and query parameters as properties, and the request body under `body`
- an output schema: the 2xx response

`make generate` also writes a tool reference for the docs. CI fails if an operation has no entry, if an entry names a missing operation, if the generated table is stale, or if an included tool's output can reach a field marked `x-pressfleet-secret`. Operations added by other milestones, such as the staging and Kubernetes work, need entries before they merge.

The proposed classification, by area:

| Area | Tools | Left out, and why |
| --- | --- | --- |
| Client groups | list, get, create, update; core policy; group backups; group backup export | Delete and offboard (irreversible) |
| Sites | list, get, create, update, start, stop, cache purge, maintenance, go-live, aliases, administrators, logs (bounded), job logs | Import and deploy (uploads); delete (irreversible); admin credentials, preview password, one-click login (secrets); log stream (stream) |
| Backups and restore | list, create, schedule, restore | Deleting a backup repository (irreversible) |
| Exports | start (site, database, backup, group backup), list, get | Downloads (binary files, for a person to fetch from the dashboard) |
| Core and rollouts | site core, policies, update, plugin and theme updates, the core, image, and template rollouts | — |
| Database sessions | list, end | Create (returns credentials) |
| Jobs | list, get, `wait_for_job` | Event stream (replaced by `wait_for_job`) |
| Monitoring | metric catalog, platform and site queries and summaries, capacity, doctor, alerts, alert rules (all but delete), alert channels (read, test), deliveries | Alert rule delete (irreversible); alert channel writes (credentials as input) |
| Users and audit | current user, list and get users, audit log | Create, update, delete users (privilege changes, irreversible); second factors, tokens, SSH keys, password, profile (credential management stays in the dashboard) |
| Platform | platform info, PHP versions | Login, logout, reauthentication, health probes |

**Annotations:**

- `readOnlyHint` on reads.
- `destructiveHint` on writes that replace or stop something but can be undone: restore, stop, and maintenance mode.
- `idempotentHint` where the operation is idempotent.
- `openWorldHint: false` everywhere.

Descriptions name the fields that hold text site users control.

### The endpoint

[#234](https://github.com/rchouinard/pressfleet/issues/234) serves Streamable HTTP at `/mcp` with the Go SDK. It is off unless `mcp.enabled` is set; with it off, `/mcp` answers `404`.

- **Routing.** `httpserver` routes `/mcp` ahead of the dashboard's single-page fallback, and exempts its streams from the write timeout, as the API's event streams are.
- **Authentication.** Requests authenticate with the bearer token through the API's own `Authenticator`; cookies are ignored. A request without a valid token gets `401` with `WWW-Authenticate: Bearer`.
- **Origin.** A request whose `Origin` header is present and not allowed is rejected, which guards against DNS rebinding (`mcp.allowed_origins`).
- **Sessions** live in memory and are bound to the token that started them. They have an idle timeout and a cap (`mcp.max_sessions`, `mcp.session_idle_timeout`), and revoking the token ends them. One control plane replica holds them all. A highly available control plane would need shared session state; it is out of scope here, as it is in the [Kubernetes plan](kubernetes-driver-plan.md).
- **Protocol.** The server supports the newest protocol revision the SDK does, and the one before it. `initialize` returns the server's name and version, and instructions that describe the tools and warn about the untrusted fields in their results.
- **Metrics:** sessions, requests by method, and tool calls by tool and result.

### Tool calls

[#235](https://github.com/rchouinard/pressfleet/issues/235) handles `tools/call`:

1. Build the operation's HTTP request from the catalog entry and the arguments, with the caller's `Authorization` header and source address.
2. Mark the request's context as an MCP call, with the client's self-reported name and version and the session ID.
3. Serve the request with the API's `http.Handler` and read the response.
4. Return the result:
   - **Success:** the response JSON as `structuredContent` and as text.
   - **A `202`:** the job.
   - **A Problem Details response:** a tool error result (`isError`) carrying the problem's fields, so the model can correct its call.

`tools/list` filters the catalog per caller with `authz.Check`, using the tool's actions and a platform target. The listing is a convenience: the call is still checked by the handler, against its real target.

The audit middleware adds `via: mcp`, the client name and version, and the session ID to the details of the events it records. As for the HTTP API, those are state-changing requests and denials; reads aren't audited.

### Jobs

[#236](https://github.com/rchouinard/pressfleet/issues/236) adds `wait_for_job`. It reads `streamJobEvents` in-process through the API handler, so it is authorized like the stream. When the client sent a `progressToken`, it sends a progress notification for each step that starts or finishes.

It returns the job once the job is final, or the job as it stands at the timeout (default 60 seconds, at most 10 minutes), with a field saying whether it finished. A canceled request ends the wait, not the job.

MCP's experimental tasks fit PressFleet jobs well, and can replace the tool once clients support them.

### Secrets and untrusted content

- **Secrets.** No tool returns a secret or takes one as input. #233 marks the secret fields in the spec, and a test proves no included tool can reach one. The end-to-end test also checks every result in its run against the secrets it knows ([#238](https://github.com/rchouinard/pressfleet/issues/238)).
- **Untrusted content.** Tool results carry text that site users control: log lines, site titles, plugin and theme names, and WordPress usernames. They come back as JSON, never as instructions. The server's instructions and the tool descriptions name those fields. The docs ([#240](https://github.com/rchouinard/pressfleet/issues/240)) explain prompt injection, and recommend read-only tokens for exploring.
- **Privilege changes are left out** because of prompt injection. An agent with a full-scope Admin token could be tricked into creating an account for an attacker, which would outlive the token's revocation (§4.6, #182).

### Dashboard

[#237](https://github.com/rchouinard/pressfleet/issues/237) adds an "MCP clients" section to the account page, shown when MCP is on. It has:

- the endpoint URL
- a shortcut to create a dedicated token, read-only by default
- configuration snippets for common clients, with the token filled in only in the dialog that shows it once
- the advice on token scopes

It also adds a filter to the audit log for events that came through MCP.

### Later: OAuth and confirmations

- **OAuth 2.1** ([#241](https://github.com/rchouinard/pressfleet/issues/241)) makes the control plane an authorization server, following MCP's authorization specification. Users approve a client in the dashboard, behind their second factor, and the grant is an ordinary API token, so authorization and audit don't change.
- **Elicitation** ([#242](https://github.com/rchouinard/pressfleet/issues/242)) brings back the irreversible operations and privilege changes. Each one needs a confirmation that the user gives in the client, and is refused when the client can't ask.

## Compatibility and release

- **Off by default.** Nothing changes for an install until `mcp.enabled` is set. The new settings follow the `config.Config` convention, with rows in `docs/configuration.md`.
- **API.** The only additions are an MCP field on `getPlatform` and the secret marks in the spec; the API version doesn't change.
- **Version skew.** The host CLI's admin interface doesn't change, so the skew rules (§4.7) are unaffected.
- **One new module**, the Go SDK, with its module graph recorded in #234.
- **Release.** It ships in a minor release.

## Issues

Sizes and priorities follow the [roadmap](roadmap.md#conventions). All are in the [Post-MVP: MCP Server](https://github.com/rchouinard/pressfleet/milestone/9) milestone and are sub-issues of [#132](https://github.com/rchouinard/pressfleet/issues/132).

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#232](https://github.com/rchouinard/pressfleet/issues/232) | Decision: MCP server design | P0 | S | — |
| [#233](https://github.com/rchouinard/pressfleet/issues/233) | MCP: tool catalog generated from the OpenAPI spec | P0 | M | #232 |
| [#234](https://github.com/rchouinard/pressfleet/issues/234) | MCP: Streamable HTTP endpoint, sessions, and configuration | P0 | M | #232 |
| [#235](https://github.com/rchouinard/pressfleet/issues/235) | MCP: tool calls through the API handler | P0 | L | #233, #234 |
| [#236](https://github.com/rchouinard/pressfleet/issues/236) | MCP: wait for jobs with progress notifications | P0 | S | #235 |
| [#237](https://github.com/rchouinard/pressfleet/issues/237) | Dashboard: connect an MCP client | P1 | M | #234 |
| [#238](https://github.com/rchouinard/pressfleet/issues/238) | E2E: MCP server acceptance tests | P0 | M | #235, #236 |
| [#239](https://github.com/rchouinard/pressfleet/issues/239) | Security review: MCP server | P0 | M | #238 |
| [#240](https://github.com/rchouinard/pressfleet/issues/240) | Docs: MCP server guide | P1 | S | #235, #236, #237 |
| [#241](https://github.com/rchouinard/pressfleet/issues/241) | MCP: OAuth 2.1 sign-in for MCP clients | P2 | L | #235, #237 |
| [#242](https://github.com/rchouinard/pressfleet/issues/242) | MCP: irreversible operations behind elicitation | P2 | M | #235 |

### Order

1. **Decisions (#232).** Then the catalog (#233) and the endpoint (#234) can be built in parallel.
2. **Tool calls (#235)**, the first usable slice: a client can list and call tools.
3. **Jobs (#236) and the dashboard section (#237).**
4. **Proof:** end-to-end tests (#238), the security review (#239), and the docs (#240).
5. **P2 follow-ups:** OAuth (#241) and elicitation (#242), once the rest has shipped.

The critical path is #232 → #233 → #235 → #236 → #238 → #239.

### Working

Each issue is worked on its own branch and merged through a pull request. PRs that change the endpoint or tool calls add the `e2e` label.

### Testing

- **Catalog:** schema conversion tests; a check that every operation is classified; the staleness check; the check that no included tool can reach a secret field.
- **Endpoint:** off by default; `Origin` checks; authentication failures, including cookies only; session limits and revocation; version negotiation. No Docker.
- **Tool calls:** `tools/list` for each role and both token scopes; denied calls; validation errors; `202`s; audit details; a test that calls every included tool once.
- **End to end:** the SDK's client against the e2e platform, covering the milestone's done-when, with every result checked against the secrets the test knows.

## Decision log

Recorded here once [#232](https://github.com/rchouinard/pressfleet/issues/232) is resolved.
