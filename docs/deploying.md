# Deploying Loft

Loft is self-hosted. A deployment is `loftd` (the API) and the `loft-web` ingress (the root site and
the static file server for deployed sites), behind a reverse proxy that authenticates users. See
[ARCHITECTURE.md](../ARCHITECTURE.md) for how the pieces fit; `docker-compose.yml` is a working
reference for the wiring, with auth off for local development.

## What you provide

- A reverse proxy that authenticates the user and forwards the headers in the
  [trust model](../ARCHITECTURE.md#trust-model).
- An OIDC provider (any standard one) for sign-in.
- A Postgres database. loftd connects as a non-superuser role so row-level security holds; the
  acceptance suite in `test/` shows the role setup.
- Object storage for uploads: a local directory (single node) or Azure Blob.
- A filesystem path that loftd and the proxy share, where deployed sites are written and served.
- An OpenAI-compatible LLM endpoint for `loft.ai`.

The defaults are platform-neutral: a local filesystem, password Postgres, and any OpenAI-compatible
endpoint. Azure bindings (Blob, managed identity) are optional, not required.

## Images

Each release publishes two images to GHCR:

- `ghcr.io/larsakerlund/loft`: the API daemon (loftd).
- `ghcr.io/larsakerlund/loft-web`: the root site and static file server (also the local-dev proxy).

## Configuration

loftd reads its configuration from the environment. [`.env.example`](../.env.example) documents every
variable, including the Azure and managed-identity alternatives. The ones a basic deployment sets:

| Variable | What |
|----------|------|
| `LOFT_OIDC_ISSUER`, `LOFT_API_AUDIENCE` | The OIDC issuer and the audience loftd validates access tokens against. |
| `LOFT_PG_CONNECTION_STRING` | Postgres connection string (or `LOFT_PG_HOST` + `LOFT_PG_USER` for managed-identity auth). |
| `LOFT_AI_ENDPOINT`, `LOFT_AI_MODEL`, `LOFT_AI_KEY` | The OpenAI-compatible LLM endpoint, model, and key. |
| `LOFT_UPLOADS_DIR` | Directory for uploads (or `LOFT_UPLOADS_ACCOUNT` + `LOFT_UPLOADS_CONTAINER` for Azure Blob). |
| `LOFT_SITES_DIR` | Where deployed sites are written and served from. |
| `LOFT_CLI_CLIENT_ID`, `LOFT_CLI_SCOPE` | The public OAuth client and scope the CLI uses. |

To let `loft login <url>` configure itself from the platform URL alone, set `LOFT_CLI_CLIENT_ID` and
`LOFT_CLI_SCOPE`. loftd serves them at `/.well-known/loft`, which the proxy must leave reachable
without authentication.
