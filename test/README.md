# Loft acceptance tests

A **language-agnostic, black-box** suite that pins the contract Loft's SDK and hosted apps depend
on. Every assertion is made over the wire (HTTP + WebSocket) against a running backend. By default it
builds and runs the **Go** `loftd` and `loft`; because the backend and CLI under test are swappable,
the same suite validates any re-implementation **without regressing any app**.

## What it covers

`acceptance.test.mjs` (backend, over HTTP/WS):

- **auth & identity**: `/api/*` requires auth (401); non-API paths 404; `/api/me` shape; identity
  keyed on the immutable `id` (survives an email/UPN change).
- **loft.db**: CRUD, `?limit`, 404/400s; **tenant isolation via RLS** (one site can't see/touch
  another's docs); **owner-only** authorization incl. **trust-on-first-use** policy locking; oversize
  rejection.
- **loft.upload**: response shape, filename sanitization, byte storage, delete (idempotent),
  malformed-path 400, cross-site delete protection.
- **loft.ai**: input validation, non-streaming `{content}`, streaming `{t}`→`{done}`, upstream
  error mapping (429→429, others→502), per-user rate limit, per-site daily token budget.
- **realtime**: `loft.db.subscribe` create/update/delete events; `loft.socket` relay (sender
  excluded); site-scoping; auth required on upgrade.

`cli.test.mjs` (CLI, hermetic): the `loft deploy` guardrails that run before any network/auth, like
empty folder, missing `index.html`, `node_modules/`, leaked `.env`, disallowed types, oversize files.

## Prerequisites

- Docker (the harness starts ephemeral **Postgres** and **llmock** containers).
- Node 18+ (test runner) and Go 1.26+ (builds the default backend/CLI under test).

The harness pulls `ghcr.io/larsakerlund/llmock` automatically if not cached. Postgres runs as a
**restricted, table-owning role** (not superuser) so row-level security is genuinely exercised. A
superuser would silently bypass RLS and make the isolation tests pass falsely.

## Run

```sh
cd test
pnpm install
pnpm test
```

## Running against a different implementation

The backend and CLI are started as swappable commands. Point the harness at any binary that honors
the same env contract (`LOFT_LISTEN`, `LOFT_PG_CONNECTION_STRING`, `LOFT_UPLOADS_DIR`,
`LOFT_AI_ENDPOINT`, `LOFT_AI_MODEL`, `LOFT_AI_KEY`, `LOFT_OIDC_ISSUER`, `LOFT_API_AUDIENCE`) and
validates the `Authorization: Bearer` access token the harness mints per request:

```sh
LOFT_BACKEND_CMD="/path/to/loftd" LOFT_CLI_CMD="/path/to/loft" pnpm test
```

If it stays green, the implementation preserves the contract and apps won't notice the switch.

## Notes

- **loft.ai upstream:** [llmock](https://github.com/larsakerlund/llmock) provides the real provider
  wire format (JSON, SSE, error envelopes, usage). loftd points at it in OpenAI-compatible mode (base
  URL + model in the body), the same path a non-Azure operator uses, so no translation shim is needed.
- **Identity:** the harness runs a mock OIDC issuer (discovery + JWKS) and signs a per-request access
  token, so the suite exercises loftd's real bearer validation. There is no header/proxy-trust path.
- **CLI happy path:** the hermetic CLI tests cover the local guardrails; a full deploy that uploads
  to a running loftd is exercised by the `deploy + delete` HTTP tests in the acceptance suite.
