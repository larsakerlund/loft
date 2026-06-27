# Loft

Self-serve static-site hosting: drop a folder, get a URL. You run `loft deploy` and the
site is live at `https://your-site.your-domain`. Inspired by Shopify's
[Quick](https://shopify.engineering/quick).

Loft is a small Go daemon (`loftd`) that gives hosted sites a backend: a schemaless document store
on Postgres with per-site row-level security, file uploads, realtime pub/sub, an LLM chat proxy, and
OIDC identity. Sites bundle the browser SDK ([`loft-js`](https://github.com/larsakerlund/loft-js))
and talk to that API same-origin.

This repo is the platform: the daemon, the root deploy site, the deploy CLI, and a local dev loop.
It runs anywhere. Cloud-specific bindings (Azure storage, blob, managed identity, an LLM provider)
are pluggable; the defaults are a local filesystem, password Postgres, and any OpenAI-compatible LLM
endpoint.

## Layout

| Path | What |
|------|------|
| `cmd/loftd` | The API daemon. |
| `cmd/loft` | The deploy CLI. |
| `internal/` | The packages behind the daemon: `db`, `uploads`, `realtime`, `ai`, `identity`, `web`, `server`, `deploy`, `config`, `limit`, `cli`. |
| `web/` | The root site: the landing page and the drop-files-to-deploy UI. Built and served as static files. |
| `test/` | Language-agnostic black-box acceptance suite (the API contract). |
| `Dockerfile` | The `loftd` image. |

## Local dev

```bash
docker compose up --build
# open http://localhost:8088          (the root site: drop a folder to deploy)
# a deployed site is then at http://<name>.localhost:8088
```

Browsers resolve `*.localhost` to `127.0.0.1`, so user sites work over plain HTTP without DNS or
certs. Deployed sites land in `./dev/sites` (a local volume, not tracked). Auth is off in this loop;
loftd takes the signed-in user from `LOFT_DEV_USER`. The `web` container is a plain nginx serving the
root site and user sites and proxying `/api` to loftd. A real deployment puts an authenticating proxy
in front instead (see "Auth contract" below).

## Tests

```bash
cd test && pnpm install && pnpm test
```

A black-box suite over HTTP/WS against the real `loftd`, with an ephemeral Postgres (a restricted,
non-superuser role so row-level security is genuinely exercised) and an LLM emulator. The backend and
CLI under test are swappable via `LOFT_BACKEND_CMD` / `LOFT_CLI_CMD`, so the suite pins the contract
across re-implementations.

## Auth contract

loftd is never trusted-open. It expects a reverse proxy in front that authenticates the user and
forwards, per request:

- `Authorization: Bearer <token>` — a validated OIDC access token minted for loftd's own API
  (audience + scope). loftd re-validates it, so the API is closed even to a caller inside the network.
  There is no header/proxy-trust identity fallback.
- `X-Loft-Site: <site>` — derived from the hostname by the proxy (the only trusted source); loftd
  scopes every query to it.
- The browser's `Sec-Fetch-Site` header, **forwarded unchanged** (do not strip it). loftd uses it to
  refuse a deploy driven from a hosted site, including a same-site subdomain whose session cookie the
  browser would still send. Stripping it would weaken that protection.

Any standard OIDC provider works, and any authenticating reverse proxy in front (oauth2-proxy is one
option). Two paths the proxy must leave reachable: `/.well-known/loft` (unauthenticated, for CLI
discovery), and a bearer-authenticated route to `/api/*` for the CLI (with oauth2-proxy,
`--skip-jwt-bearer-tokens` lets a valid bearer through instead of requiring a browser session).

## Deploying with the CLI

```bash
npx loft-cli login https://loft.example.com   # discovers OAuth settings from the URL, device-flow sign-in
npx loft-cli deploy ./dist --name blog        # upload a folder; serves at https://blog.<your-domain>
npx loft-cli delete blog
```

Setup is just the platform URL: `loft login <url>` reads the OAuth config from the platform's
`/.well-known/loft` (over HTTPS, so TLS authenticates it) and signs you in via the OAuth device flow,
saving the token. `loft deploy` then validates the folder locally (entry point, size, no project
cruft or secrets) and uploads it to `/api/deploy`, which re-checks the limits server-side. For CI,
set `LOFT_TOKEN` to a bearer token instead of running `login`.

To advertise CLI discovery, a deployment sets `LOFT_CLI_CLIENT_ID` (a public OAuth client id) and
`LOFT_CLI_SCOPE`; the issuer comes from `LOFT_OIDC_ISSUER` (or `LOFT_TENANT_ID` for Entra).

## SDK

Hosted apps bundle [`loft-js`](https://github.com/larsakerlund/loft-js): `loft.user`, `loft.db`,
`loft.upload`, `loft.socket`, `loft.ai`. The daemon serves the API; apps bundle the SDK.
