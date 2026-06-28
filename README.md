<div align="center">
  <img src="web/src/assets/loft-logo.png" alt="Loft" width="96" height="96" />
  <h1>Loft</h1>
  <p><strong>Drop a folder, get a URL.</strong> Self-serve static-site hosting with a real backend.</p>
</div>

---

Loft hosts static sites and gives each one a backend, so a folder of HTML, CSS, and JavaScript can
do real work without you standing up a server. You run one command:

```bash
loft deploy ./dist --name blog
# ✓ deployed 12 files to https://blog.your-domain
```

The site is live, and the JavaScript inside it can store data, take uploads, stream realtime
messages, and call an LLM, all from the browser against an API on the same origin. No build server,
no per-app infrastructure. Inspired by Shopify's [Quick](https://shopify.engineering/quick).

## What a hosted site gets

Each deployed site bundles the browser SDK ([`loft-js`](https://github.com/larsakerlund/loft-js)) and
talks to its backend same-origin. The whole API is one object:

```js
import loft from "loft-js";

// Who is signed in (the proxy in front authenticates the user).
const me = await loft.user.me();

// A schemaless document store, scoped to this site. Other sites cannot read it.
const posts = loft.db.collection("posts");
await posts.create({ title: "Hello", body: "..." });
const all = await posts.list();

// File uploads, stored server-side, returned as a URL you can drop into <img src>.
const { url } = await loft.upload(file);

// Realtime pub/sub between visitors.
const room = loft.socket.channel("lobby");
room.on((msg) => render(msg));
room.send({ hello: "there" });

// An LLM, streamed, with no API key in the client.
const prompt = [{ role: "user", content: "Summarize this page" }];
for await (const token of loft.ai.stream(prompt)) print(token);
```

Data is isolated per site by Postgres row-level security, so one tenant can never read another's
rows. The LLM key, the database, and the storage credentials live in the daemon, never in the
browser.

## Try it locally

```bash
docker compose up --build
# open http://localhost:8088          the root site: drop a folder to deploy
# a deployed site is then at http://<name>.localhost:8088
```

Browsers resolve `*.localhost` to `127.0.0.1`, so deployed sites work over plain HTTP with no DNS and
no certificates. Deployed sites land in `./dev/sites` (a local volume, not tracked). This loop runs
with auth off for speed: loftd takes the signed-in user from `LOFT_DEV_USER`, and a plain nginx
serves the root site and user sites while proxying `/api` to loftd. A real deployment puts an
authenticating proxy in front instead (see [Auth contract](#auth-contract)).

## Deploy to a real platform

The CLI ships on npm and is a single binary, so no install step is needed:

```bash
npx loft-cli login https://loft.example.com   # discovers OAuth settings from the URL, then signs you in
npx loft-cli deploy ./dist --name blog        # serves at https://blog.<your-domain>
npx loft-cli delete blog
```

Setup is just the platform URL. `loft login <url>` reads the OAuth config from the platform's
`/.well-known/loft` (over HTTPS, so TLS authenticates it) and signs you in with the OAuth device
flow, saving the token. `loft deploy` then validates the folder locally (entry point, size, no
project cruft or secrets) and uploads it to `/api/deploy`, which re-checks the limits server-side.
For CI, set `LOFT_TOKEN` to a bearer token instead of running `login`.

A deployment advertises CLI discovery by setting `LOFT_CLI_CLIENT_ID` (a public OAuth client id) and
`LOFT_CLI_SCOPE`; the issuer comes from `LOFT_OIDC_ISSUER`. See [`.env.example`](.env.example) for the
full set of knobs (database, identity, LLM, storage). The defaults are platform-neutral: a local
filesystem, password Postgres, and any OpenAI-compatible LLM endpoint. Cloud bindings (Azure storage,
blob, managed identity) are pluggable, not required.

## How it fits together

Loft is a small Go daemon, `loftd`, that serves only the backend API. It is never trusted-open: a
reverse proxy in front authenticates the user and forwards a validated token, the daemon re-validates
it, and the proxy serves the static files. The pieces:

| Path | What |
|------|------|
| `cmd/loftd` | The API daemon. |
| `cmd/loft` | The deploy CLI. |
| `internal/` | The packages behind the daemon: `db`, `uploads`, `realtime`, `ai`, `identity`, `web`, `server`, `deploy`, `config`, `limit`, `cli`. |
| `web/` | The root site: the landing page and the drop-files-to-deploy UI. Built and served as static files. |
| `test/` | A language-agnostic black-box acceptance suite (the API contract). |
| `npm/` | The `loft-cli` npm packages: a launcher plus a prebuilt binary per platform. |
| `Dockerfile` | The `loftd` image. |

### Auth contract

loftd expects a reverse proxy in front that authenticates the user and forwards, per request:

- `Authorization: Bearer <token>`, a validated OIDC access token minted for loftd's own API
  (audience + scope). loftd re-validates it, so the API is closed even to a caller inside the
  network. There is no header- or proxy-trust identity fallback.
- `X-Loft-Site: <site>`, derived from the hostname by the proxy (the only trusted source); loftd
  scopes every query to it.
- The browser's `Sec-Fetch-Site` header, **forwarded unchanged** (do not strip it). loftd uses it to
  refuse a deploy driven from a hosted site, including a same-site subdomain whose session cookie the
  browser would still send. Stripping it would weaken that protection.

Any standard OIDC provider works, with any authenticating reverse proxy in front (oauth2-proxy is one
option). Two paths the proxy must leave reachable: `/.well-known/loft` (unauthenticated, for CLI
discovery), and a bearer-authenticated route to `/api/*` for the CLI (with oauth2-proxy,
`--skip-jwt-bearer-tokens` lets a valid bearer through instead of requiring a browser session).

## Tests

```bash
cd test && pnpm install && pnpm test
```

A black-box suite over HTTP and WebSocket against the real `loftd`, with an ephemeral Postgres (a
restricted, non-superuser role, so row-level security is genuinely exercised) and an LLM emulator.
The backend and CLI under test are swappable via `LOFT_BACKEND_CMD` and `LOFT_CLI_CMD`, so the suite
pins the contract across re-implementations.

## Releases

Releases run on [Conventional Commits](CONTRIBUTING.md). Merging the release PR that
[release-please](https://github.com/googleapis/release-please) keeps open tags the version and
publishes:

- `ghcr.io/larsakerlund/loftd` and `ghcr.io/larsakerlund/loft-web`: the daemon and root-site images.
- [`loft-cli`](https://www.npmjs.com/package/loft-cli) on npm, with a prebuilt binary per platform.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the commit and code conventions, and `pre-commit install`
to wire up the hooks. Licensed under [MIT](LICENSE).
