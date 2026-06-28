# Architecture

`loftd` is a single Go static binary that serves an HTTP API and nothing else. A reverse proxy in
front of it authenticates the user and serves the static files; loftd holds the data, the storage,
and the keys.

```
            ┌─────────────── reverse proxy ───────────────┐
 browser ──▶│ authenticates the user                      │
            │ serves the root site, deployed sites, and   │
            │ uploaded files                              │
            │ forwards /api/* to ▼                        │
            └──────────────────┬──────────────────────────┘
                               ▼
                             loftd ──▶ Postgres, object storage, LLM endpoint
```

## Trust model

loftd is never trusted-open. It expects the proxy to authenticate the user and forward, per request:

- `Authorization: Bearer <token>`, a validated OIDC access token minted for loftd's own API
  (audience + scope). loftd re-validates it, so the API is closed even to a caller inside the
  network. There is no header- or proxy-trust identity fallback.
- `X-Loft-Site: <site>`, derived from the hostname by the proxy (the only trusted source). loftd
  scopes every query to it.
- The browser's `Sec-Fetch-Site` header, forwarded unchanged. loftd uses it to refuse a deploy driven
  from a hosted site, including a same-site subdomain whose session cookie the browser would still
  send. Stripping it would weaken that protection.

Any standard OIDC provider works, behind any authenticating reverse proxy (oauth2-proxy is one
option). The proxy must leave two paths reachable: `/.well-known/loft` (unauthenticated, for CLI
discovery) and a bearer-authenticated route to `/api/*` for the CLI (with oauth2-proxy,
`--skip-jwt-bearer-tokens` lets a valid bearer through instead of requiring a browser session).

## Data isolation

Every document a site stores lives in Postgres under row-level security, keyed by the `X-Loft-Site`
the proxy sets. loftd connects as a non-superuser role, so even buggy application code cannot read
another site's rows: the database enforces the boundary. The acceptance suite in `test/` runs against
the same restricted role, so the isolation it checks is the one a deployment uses.

## Repository layout

| Path         | What                                                                                               |
| ------------ | -------------------------------------------------------------------------------------------------- |
| `cmd/loftd`  | The API daemon.                                                                                    |
| `cmd/loft`   | The deploy CLI.                                                                                    |
| `internal/`  | The packages behind the daemon.                                                                    |
| `web/`       | The root site: the landing page and the drop-files-to-deploy UI, built and served as static files. |
| `test/`      | A language-agnostic black-box acceptance suite that pins the API contract.                         |
| `npm/`       | The `loft-cli` npm packages: a launcher plus a prebuilt binary per platform.                       |
| `Dockerfile` | The `loftd` image.                                                                                 |
