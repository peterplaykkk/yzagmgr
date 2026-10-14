# yzagmgr — self-hosted AI gateway & local AI-tool configuration manager

yzagmgr is a single Go daemon that plays two roles for people who run AI coding
agents — and for the teams behind them.

**As a self-hosted AI gateway**, it gives a fleet of agents — many users, many
concurrent sessions — one unified, local egress. Clients call it instead of the
provider directly; it forwards traffic to the official upstreams (GLM, OpenAI,
Anthropic), attributes every request to a user and a session, and governs the
traffic with token quotas, RPM/TPM rate limits and a billing ledger that
supports time-of-day price multipliers and peak-hour rules.

**As a local configuration manager**, it keeps the configuration of the AI
tools installed on a machine — Claude Code, Claude Desktop, Codex, Gemini CLI,
Grok Build, OpenCode, OpenClaw, Hermes, Pi — organized as switchable profiles,
and edits their live config files transactionally: preview first, apply with a
one-shot lease, byte-faithful merges, automatic pre-write backups, and
one-command restore. Provider credentials live in a managed store; config
files only ever carry opaque references.

The same process serves a React web workbench (usage, monitoring, pricing,
rate limits, routing, diagnostics, settings), a `yzagmgr` CLI for headless
machines, and a Go SDK that embeds the identical routing engine in your own
application.

中文说明: [README_ZH.md](README_ZH.md) · License: [0BSD](LICENSE)

| Component | What it is |
| --- | --- |
| `cmd/yzagmgr` | The daemon and CLI: management API + data-plane proxy + usage/billing store, in one CGO-free static binary |
| Web workbench | React + MUI SPA, served by the daemon on the management port |
| Go SDK | `sdk/router` (embed the same routing engine) + `sdk/store` (SQLite / PostgreSQL / MySQL persistence, token vault) |
| Contracts | [`doc/api-v1.md`](doc/api-v1.md) (HTTP API v1), [`doc/cli.md`](doc/cli.md) (CLI), [`doc/glm-pricing.md`](doc/glm-pricing.md) (GLM price pinning) |

## Why yzagmgr

### What you get

- **One accountable egress for many agents.** Every request through the data
  plane is attributed to a user and a session and recorded in one place:
  tokens, status, duration, model requested vs. model sent, and the routing
  rule version that produced the decision. Usage aggregates by user, session
  and model, with trends.
- **Limits that act before the upstream does.** Token quotas (per user and per
  session) and per-user RPM/TPM rate limits reject an offending request with
  `429` *before the request body is read and before any upstream request is
  made* — and stay fail-open when the usage store cannot be read, so a broken
  database never turns into a global outage.
- **Billing you can reconcile.** A per-model price table plus rule-based
  time-window multipliers (factory preset: GLM peak hours 14:00–18:00 at ×3 of
  the list price) produce a write-time-snapshot ledger; changing prices or
  rules never rewrites history. Real charged amounts and base-price estimates
  are shown side by side per user.
- **Config management with an undo button.** Nine AI tool families switch
  between profiles (official endpoint, local gateway, or a custom endpoint)
  through a preview → apply pipeline with byte-faithful merges, automatic
  backups, and byte-exact restore.
- **Credential custody.** Provider keys and managed tokens live once, in 0600
  files under the data root (plus a managed token store); tool config files
  carry only `yzagmgr-managed-token://…` references, and nothing sensitive
  reaches logs or API responses.

### When it fits

- You run many agent sessions (per user, per task, per machine) and want usage
  and cost attributable per user/session without each client holding provider
  API keys.
- You want one machine (or VM, or container) to be the only place provider
  credentials exist, while other sessions reach the providers through it.
- You keep switching an AI tool between a local gateway and its original
  official endpoint, and you are tired of hand-editing JSON/TOML/YAML with no
  backup and no way back.
- You need quota and rate-limit governance plus a chargeback-style statement
  whose peak-hour pricing rules match the upstream's official time-of-day
  billing.

## Features

### Gateway (data plane)

- **Arm routing — per-user, per-session egress.**
  `http://<bind>:<proxy_port>/router/u-<user>-<session>-<provider><client path>`
  routes a request to an official upstream while carrying its identity. The
  arm name is a single path segment (≤ 96 bytes); `<user>` and `<session>` are
  caller-chosen slugs (`[A-Za-z0-9._-]{1,128}`, not starting with `.`, no
  `..`); `<provider>` is a closed set: `glm`, `openai`, `anthropic`. Two alias
  forms (`/u/<user>/<session>/<provider>…` and the root-level
  `/u-<user>-<session>-<provider>…`) parse identically. The request path then
  picks the upstream wire: `/v1/messages…` → Anthropic Messages;
  `/chat/completions` and `/v1/chat/completions` → chat completions;
  `/responses`, `/v1/responses` and any unregistered path → responses. Arm
  traffic is pure pass-through: the request body and the upstream response go
  out and back byte-for-byte with the upstream status code, no model or field
  rewriting. Malformed arms, retired providers and unsupported provider×wire
  combinations are a JSON `404` — never a silent fallback to some default
  route.
- **Body-model router (request mappings).** Clients that just speak the usual
  provider protocols can point their base URL at `…/router`; yzagmgr selects
  a configured upstream ("request mapping") by the model name in the request
  body, applies single-hop alias → target model rewrites and reasoning-effort
  remapping (`none|minimal|low|medium|high|xhigh|max`, plus a "force" mode),
  and fails closed (4xx, no fallback) when nothing matches. `POST
  /routes/explain` runs the exact same decision code as a pure preview — no
  network, no disk, no leases — and reports which rule would match, what model
  would be sent out, and any rule conflicts, before you apply anything.
- **Official upstreams, three wires per provider.** The supported
  provider × wire matrix (built-in; per-line [base override](#upstreams--pricing)
  available):

  | Client path (after the arm) | Wire | `glm` | `openai` | `anthropic` |
  | --- | --- | --- | --- | --- |
  | `/v1/messages`, `/v1/messages/…` | Anthropic Messages | `https://open.bigmodel.cn/api/anthropic` | not supported (404) | `https://api.anthropic.com` |
  | `/chat/completions`, `/v1/chat/completions` | chat completions | `https://open.bigmodel.cn/api/paas/v4` | `https://api.openai.com/v1` | not supported (404) |
  | `/responses`, `/v1/responses`, unregistered paths | responses | `https://open.bigmodel.cn/api/v1` | `https://api.openai.com/v1` | not supported (404) |

  Version segments already included in a base are never duplicated: one extra
  `/v1` on `/v1/responses` or `/v1/chat/completions` is flattened; every other
  path goes upstream unchanged.
- **Credential injection with an explicit placeholder.** A real key in the
  client's credential headers passes through untouched. The placeholder
  `PROXY_MANAGED` (or a missing credential) resolves per request through the
  chain: `providers.json` `credRef` → managed token store → environment
  fallback. Resolution failure is a `401` without contacting the upstream, and
  the placeholder itself is never sent upstream.
- **Metering.** One record per request — user, session, app family, provider,
  model requested/upstream, status, duration, input/output tokens, streaming,
  rule version. Aggregates by user/user-list/model/session, time-window
  trends with rpm/tpm, and a request-detail search surface.
- **Token quotas, enforced up front.** Quota rows are per user and per
  session (arm). The check happens after the arm identity is parsed and before
  the request body is read: over quota ⇒ `429 quota_exceeded` with
  `quota`/`used_total`/`remaining` in the body, zero upstream requests, and
  the rejected attempt still recorded. Usage is recomputed from the request
  log (single source of truth); a reset baseline lets you zero the effective
  usage without rewriting history. No row, `NULL` or `0` means unlimited;
  an unreadable store fails open.
- **Rate limits.** Per-user RPM/TPM limits over natural-minute windows;
  violations return `429 rate_limited` before any upstream request and heal
  by themselves at the next minute boundary.
- **Billing ledger.** Every billed request lands in a ledger as a write-time
  snapshot of tokens × prices, the applied multiplier and currency — changing
  prices or rules never rewrites the past. Multi-currency rows bucket
  separately (no FX conversion), and unpriced models are flagged instead of
  being charged 0. See [Upstreams & pricing](#upstreams--pricing).
- **Monitoring read surface.** `/monitor/overview`, `/monitor/trends` and
  `/monitor/session-cost` return a fixed metric dictionary for an upstream
  dashboard. Anything that cannot be measured is an explicit `null` plus a
  `missing` entry — never a silent zero. Runtime performance is measured from
  the data plane only (no fabricated TTFT / cache-hit figures).

### Configuration manager

- **Nine tool families**, one uniform contract: Claude Code, Claude Desktop,
  Codex, Gemini CLI, Grok Build, OpenCode, OpenClaw, Hermes, Pi. For each
  tool, its *profile* (tier) is classified from the bytes actually on disk
  (`official` / `gateway` / `absent` / `drift` / `unknown`), and its
  *installed state* (`installed` / `not_installed` / `unknown`) is judged
  separately from installation evidence. Disagreement between disk and
  internal bookkeeping is reported as `drift`, never smoothed over; probe
  failure is `unknown`, never faked into a definite answer.
- **Transactional writes.** `preview` computes the full diff and creates a
  one-shot lease (180 s); `apply` re-checks the file fingerprint, writes
  atomically, and rolls back per file on failure. If a file changed since the
  preview, apply refuses with `config_changed` and writes nothing. Merges are
  byte-faithful — formatting, key order and unrelated keys survive — across
  JSON, JSON5, TOML and YAML.
- **Backups and restore.** Every write keeps its pre-write bytes; restore by
  backup id, or "undo the most recent write", returns files byte-exactly to
  where they were.
- **Managed credential store.** Plaintext tokens enter only through 0600
  files or the CLI; configs store opaque `yzagmgr-managed-token://<uid>`
  references. No read-back path emits plaintext.
- **MCP registry.** Manage MCP servers once
  (`<data_root>/managed/mcp-servers.json`, 0600, atomic writes) and
  materialize them into the Claude Code and Claude Desktop config files with
  idempotent, byte-faithful merges: concurrent unrelated edits are absorbed,
  edits to our own entries since the preview are reported as conflicts with
  zero writes.
- **Usage scripts, sandboxed.** A per-profile JavaScript script (isolated
  runtime, 5 s timeout, 16 MiB heap, 256 KiB stack, no host capabilities) can
  query a provider's quota endpoint using opaque credential placeholders
  (`{{apiKey}}`, `{{accessToken}}`, `{{baseUrl}}`, `{{userId}}`). Targets must
  be HTTPS (localhost is not exempt) and pass an SSRF blacklist plus an
  approved-origin allowlist; credentials are only substituted into read-only
  requests to approved origins, and trial runs are zero-persistence.
- **Environment-conflict scan and repair.** Detects stale/conflicting
  environment variables fed to tools (shell startup files, launchd, process
  environment), repairs by deleting the exact offending lines with byte
  snapshots and a restore path. `/etc/*` is checked read-only, never written.
- **Diagnostics.** Snapshot and probes (config readability, proxy state,
  upstream connectivity classes), a downloadable diagnostic bundle with
  personal paths masked, and fix *plans* — the plan itself performs zero
  writes and every fix requires explicit confirmation.

### Web workbench

The daemon serves the SPA on the management port; it injects the admin token
into the page (`<meta name="agmgr-token">`) and the UI keeps it in memory.

| Page | Route | What it does |
| --- | --- | --- |
| Usage | `/usage` | Tabs: **Trends** (interactive monitor charts with window/bucket/model filters), **Model stats**, **Requests** (request-level detail and filters), **Billing** (ledger and per-user statements), **Pricing** (price table + time-window rules) |
| Request mapping | `/arms` | Manage request mappings (arms), model aliases and effort tables; run decision explanations |
| Users | `/users` | Per-user usage summaries; RPM/TPM and session-quota editors |
| System | `/system` | Local data usage and retention, table row counts, query latency |
| Diagnostics | `/diagnostics` | Environment/diagnosis snapshot, probes, diagnostic bundle, confirmation-gated fix plans |
| Settings | `/settings` | General, credentials, operations (operation log), help |

Built with React 19, MUI 7, Vite 7, TypeScript 5.9 and visactor/VChart;
bilingual UI (English / Chinese).

### CLI and Go SDK

- **CLI** (`yzagmgr`) — headless control for everything the workbench does:
  `health`, `tools list/show`, `preview`/`apply`/`restore`, `backups list`,
  `routes`/`remap`, `profiles`, `policy`, `materialize`, plus `--json`
  output that matches the REST envelope. Exit codes distinguish usage errors,
  business rejections, a missing token, and an unreachable daemon. Full
  reference: [`doc/cli.md`](doc/cli.md).
- **Go SDK** — `sdk/router` starts the same routing engine (arm routing,
  aliases, effort resolution, token resolution) inside your own process, and
  `sdk/store` persists routes / aliases / effort tables / tokens to SQLite,
  PostgreSQL or MySQL with an encryption-at-rest token vault (`TokenCipher`,
  standard library only). An example lives in `sdk/examples/local-router`.

## Quick start

### Requirements

| Dependency | Requirement |
| --- | --- |
| Go | 1.25+ (module declares `go 1.25.0`; everything is CGO-free) |
| Node.js | `^20.19` or `>=22.12` (Vite 7 requirement) — only to build the web UI |
| PostgreSQL | Optional (default storage is embedded SQLite) |
| Chromium | Optional — only for browser E2E tests |

### Build and run

```sh
# 1) build the web UI first (the daemon serves web/dist at runtime)
cd web && npm ci && npm run build && cd ..

# 2) build the daemon (static, pure Go)
CGO_ENABLED=0 go build -o bin/yzagmgr ./cmd/yzagmgr   # or: make build

# 3) run it
./bin/yzagmgr
```

The daemon then listens on:

| Listener | Default | Env | Serves |
| --- | --- | --- | --- |
| Management API + web workbench | `127.0.0.1:17022` | `YZAGMGR_API_PORT` | `/api/v1`, `/monitor`, `/quota`, the SPA — open `http://127.0.0.1:17022/` in a browser |
| Data plane | `127.0.0.1:17021` | `YZAGMGR_PROXY_PORT`, `YZAGMGR_PROXY_BIND` | `/router/…` and arm paths |

(Routing paths are also answered on the management port, so a mistyped client
path gets a JSON 404 instead of silently hitting the web UI.) Ports are
strictly checked at startup: if a port is taken, the daemon fails — it never
silently picks another one, because ports are written into tool configs.

Data root: `~/.yzagmgr/data` (see [Configuration](#configuration)):

```
~/.yzagmgr/data/
├── admin.token        # management token (0600, generated on first start)
├── providers.json     # upstream registry (0600, optional)
├── usage.db           # SQLite usage/billing store (0600)
├── logs/yzagmgr.log   # bounded log (1 MiB × 2 rotations)
├── backups/           # per-write config backups
└── managed/           # profiles, MCP registry, policy, records
```

### Front the API

The management API requires the token from `<data_root>/admin.token`:

```console
$ curl -H "X-Agmgr-Token: $(cat ~/.yzagmgr/data/admin.token)" \
       http://127.0.0.1:17022/api/v1/tools
$ bin/yzagmgr health          # CLI reads the same token file automatically
```

### First requests through the gateway

Give the gateway an upstream key (environment fallback, or via
`providers.json` / the managed token store), then call an arm path — the
client needs no provider credential of its own:

```console
$ export YZAGMGR_UPSTREAM_GLM_KEY=sk-…
$ curl http://127.0.0.1:17021/router/u-alice-s1-glm/v1/messages \
    -H 'content-type: application/json' \
    -d '{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

Note on the single-segment arm form: the last `-` splits `<user>` from
`<session>`, so make the session id dash-free (a 16-hex id works well); the
multi-segment alias form `/u/<user>/<session>/<provider>…` has no such
restriction.

Or register a request mapping so a client can just point its base URL at
`http://127.0.0.1:17021/router` and let the model name in the request body
select the upstream:

```console
$ printf 'sk-…' > glm.token && chmod 600 glm.token
$ bin/yzagmgr routes add glm --endpoint https://open.bigmodel.cn/api/paas/v4 --cred-file glm.token
$ bin/yzagmgr remap model set glm --alias my-fast --target glm-5.3-flash
```

## Configuration

All configuration is via environment variables; the startup set is read once
at assembly time (nothing at runtime can change it), while the upstream key
fallbacks are read per request.

| Variable | Default | Meaning |
| --- | --- | --- |
| `YZAGMGR_HOME` | — | Isolation master switch: the data root and the managed tools' `$HOME` base both point under it (used by tests and E2E; also handy for containers) |
| `YZAGMGR_API_PORT` | `17022` | Management API port (loopback only) |
| `YZAGMGR_PROXY_PORT` | `17021` | Data-plane port |
| `YZAGMGR_PROXY_BIND` | `127.0.0.1` | Data-plane bind address — a loopback / private / link-local **IP literal** only (e.g. a container bridge address); public addresses and domain names are rejected |
| `YZAGMGR_DB_DSN` | — (SQLite) | PostgreSQL connection string for the usage/billing store; when set, an existing SQLite database is migrated once (with reconciliation) |
| `YZAGMGR_WEB_DIR` | auto-detected `web/dist` | Directory of the built web UI |
| `YZAGMGR_POLICY_FILE` | — | Seed a policy file (default model, model catalog); the policy source is a local file — zero network |
| `YZAGMGR_USAGE_MAINTENANCE` | on | `off`/`0`/`false` disables the periodic usage-store maintenance (test switch) |
| `YZAGMGR_UPSTREAM_GLM_KEY` | — | Fallback upstream key for `glm` (read per request) |
| `YZAGMGR_UPSTREAM_OPENAI_KEY` | — | Fallback upstream key for `openai` (read per request) |
| `YZAGMGR_UPSTREAM_ANTHROPIC_KEY` | — | Fallback upstream key for `anthropic` (read per request) |

### providers.json

`<data_root>/providers.json` (0600, hot-reloaded, atomically written) is the
upstream registry, readable and writable through the admin API
(`GET`/`PUT /api/v1/providers`):

```json
{
  "providers": {
    "glm": {
      "enabled": true,
      "bases": { "chat": "https://open.bigmodel.cn/api/paas/v4" },
      "credRef": "yzagmgr-managed-token://mt-…"
    },
    "openai": { "enabled": true },
    "anthropic": { "enabled": false }
  }
}
```

- `enabled: false` rejects all arm traffic for that provider with an
  identifiable "disabled" 404 (distinct from "retired provider").
- `bases` overrides the built-in base URL per wire line (`anthropic`, `chat`,
  `responses`) — useful for a proxy, a gateway or a grey release.
- `credRef` points into the managed token store. Resolution order per
  request: a real client-supplied key (pass-through) → `credRef` → environment
  fallback. A `providers.json` that is group/world-readable, malformed, or
  missing `providers` is refused with a warning and the environment fallback
  still serves — a corrupted file is not treated as "no credentials".

## Upstreams & pricing

Three official providers are built in — `glm` (Zhipu/BigModel), `openai`,
`anthropic` — with the per-wire base URLs shown in
[Features](#gateway-data-plane). Request-mapping endpoints, by contrast, can
be any URL you configure (e.g. a self-hosted OpenAI-compatible gateway).

Billing has two layers:

1. **Price table** (`GET`/`PUT /api/v1/usage/pricing`) — per model:
   input, output and cache-read prices per million tokens, plus a currency
   (e.g. `CNY`). Multi-currency rows are never converted; they bucket
   separately.
2. **Pricing rules** (`GET`/`PUT /api/v1/usage/pricing/rules`) — time-window
   multipliers applied on top of the price table (rules match in priority
   order, ties broken by id — first match wins):

   ```json
   {
     "id": "glm_peak_x3",
     "priority": 0,
     "matchProvider": "glm",
     "matchModelPrefix": "",
     "multiplierMicro": 3000000,
     "windowStartMin": 840,
     "windowEndMin": 1080,
     "timezone": "",
     "active": 1,
     "note": "GLM peak window 14:00–18:00 local, ×3"
   }
   ```

   `840`–`1080` are local minutes (14:00 ≤ t < 18:00, closed-open); the
   multiplier is exact (`×1e6` integer, here ×3); `start == end` (including
   both `0`) means all day; `active: 0` disables without deleting. This
   `glm_peak_x3` preset ships with every fresh database, charging GLM traffic
   in the peak window at ×3 of the configured base prices — the GLM base
   prices themselves are pinned to the official list prices in
   [`doc/glm-pricing.md`](doc/glm-pricing.md).

Cost accounting is a write-time snapshot: base cost = input × input price +
output × output price (+ cached × cache-read price where a model declares
one), × any matching window multiplier; the per-row result lands in the
billing ledger together with the prices, multiplier and currency used.
Changing prices or rules affects only future rows. `/api/v1/usage/ledger`
lists rows; `/api/v1/usage/statement/{user}` compares charged amounts against
a base-price estimate for the same window and reports the number of unpriced
requests.

## HTTP API

The management API lives at `http://127.0.0.1:17022/api/v1` (port
overridable). The contract is frozen in [`doc/api-v1.md`](doc/api-v1.md) —
the web UI, the CLI, scripts and the contract tests all call exactly the same
endpoints; there is no privileged channel.

- Envelope: success `{"ok":true,"data":…}`, failure
  `{"ok":false,"error":{"code","message","details"},"requestId":…}`; the
  error-code set is closed (unknown ids included).
- Auth: `X-Agmgr-Token` on every request except `GET /health` and
  `GET /version`; cross-site origins are rejected.
- Pagination: `?limit=<1..200>&cursor=…`; list responses carry `items`,
  `nextCursor`, `total`.
- Outside `/api/v1` but still token-guarded: the monitoring read surface
  (`/monitor/overview`, `/monitor/trends`, `/monitor/session-cost` — bare
  JSON, no envelope) and the quota surface (`/quota/{slug}`).
- Outside `/api/v1` and deliberately unauthenticated: the data plane (arm
  paths and `/router/…`), which authenticates nothing and attributes
  everything.

Some wire-level names deliberately keep the short `agmgr` spelling for
compatibility — the request header `X-Agmgr-Token`, the `<meta
name="agmgr-token">` tag, the token file name `admin.token`, the default
ports. Treat them as frozen.

## Security model

- **Local-only management surface.** The management API always binds
  `127.0.0.1`. Startup fails rather than silently changing ports.
- **Admin token.** 32 random bytes (hex) in `<data_root>/admin.token`
  (0600), generated on first start and reused afterwards; compared in
  constant time. The served page receives it via a meta tag and keeps it in
  memory only — never in cookies, URLs or localStorage. Cross-site requests
  are rejected (`forbidden_origin`) and no CORS headers are emitted.
- **No accounts, on purpose.** yzagmgr ships **no user login, account or
  subscription system**. Identity is asserted by the caller: arm paths carry
  `<user>`/`<session>` slugs, and the quota/monitoring surfaces are guarded
  only by the admin token. Run it loopback-only (or bind the data plane to a
  private address) and place it behind the component that already
  authenticates your users.
- **Credentials.** Upstream keys and managed tokens live in 0600 files under
  the data root and in the managed token store; tool config files carry only
  opaque `yzagmgr-managed-token://` references; keys are never written to
  logs or API responses; the `PROXY_MANAGED` placeholder is never sent
  upstream; the daemon never touches the system keychain.
- **Data plane.** Unauthenticated by design — it attributes requests, it does
  not authenticate clients. It binds loopback by default; a non-loopback bind
  is allowed only as a loopback/private/link-local IP literal and logs a
  warning.
- **Sandboxed scripts.** Usage scripts run in an isolated JavaScript runtime
  with a 5 s timeout, 16 MiB heap and 256 KiB stack, no host capabilities,
  HTTPS-only targets, an SSRF blacklist and an approved-origin allowlist;
  secrets are substituted only into read-only requests to approved origins.
- **Narrow logs.** The process log is bounded (1 MiB × 2 rotations).
  Authentication rejections leave a request id and a reason — never a token
  value. Operation logs record mutations, not secrets.
- **Env repair never writes `/etc`.** Environment-conflict repair touches
  shell startup files with byte snapshots and a restore path; `/etc/*` is
  inspected read-only.

## Development

```sh
# Go: build + tests (static, CGO-free)
export TMPDIR="$HOME/tmp-yzagmgr-test"   # short, symlink-free path — see below
go build ./... && go vet ./...
go test ./... -count=1                   # or: make test

# Web unit tests and production build
cd web && npm ci && npm test && npm run build && npm run typecheck
```

- **Why `TMPDIR`?** Some tests bind unix sockets. On macOS the default
  `$TMPDIR` lives under the very long `/var/folders/…` path (and `/var` is a
  symlink), which can overflow the 104-byte `sun_path` limit and produce
  false failures; a short real directory fixes it. On Linux the default is
  fine.
- PostgreSQL/MySQL integration tests skip themselves unless you provide
  `YZAGMGR_TEST_PG_DSN` / `YZAGMGR_SDK_MYSQL_DSN`; the SDK's PostgreSQL suite
  starts a throwaway instance when `initdb`/`pg_ctl` are on `PATH`.
- The web unit suite runs through a small memory-guard wrapper
  (`scripts/test-mem-guard.sh`), already wired into `npm test`.
- Browser E2E: `make e2e` builds a real daemon from source and drives it with
  a real Chromium (`npx playwright install chromium` once). It needs the Go
  toolchain on `PATH` and is deliberately not part of `make gates`.
- `make gates` runs the low-level gates: formatting, vet, build, full Go
  suite plus the web checks.
- `scripts/verify/` contains self-contained behavioral acceptance scripts —
  each starts an isolated daemon data root with hermetic upstream stubs;
  missing prerequisites report an explicit `BLOCKED`, never a fake green.
- Cross-compiling is trivial (pure Go including the SQLite driver):
  `GOOS=linux GOARCH=amd64 go build ./cmd/yzagmgr`.
- A multi-stage Dockerfile is included: it builds the web bundle with Node,
  the daemon with Go (architecture-aware, CGO-free) and ships a non-root
  runtime image; set `YZAGMGR_HOME` and mount a volume there to persist the
  data root.

## License

0BSD — see [LICENSE](LICENSE). Third-party dependency licenses:
[THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md).
