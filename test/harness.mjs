// Black-box test harness for the Loft backend (loftd). It stands up everything the backend talks
// to and starts the backend itself as a SWAPPABLE command, then exposes HTTP/WS helpers that carry a
// signed Bearer (the access token a real proxy forwards) plus the trusted X-Loft-Site header. Because
// every assertion is over the wire (HTTP + WebSocket), the exact same suite validates the current
// loftd and a re-implementation: point LOFT_BACKEND_CMD at the other binary and nothing else changes.
//
// What it manages:
//   • Postgres (ephemeral container) with a RESTRICTED, non-superuser role that OWNS the tables.
//     This is essential: a superuser bypasses row-level security, which would make the tenant-
//     isolation tests pass falsely. The restricted owner mirrors prod's managed-identity role, so
//     FORCE RLS is genuinely exercised.
//   • llmock (the project's LLM emulator) for loft.ai, with real provider wire format, SSE, error
//     envelopes and usage, all deterministic. loftd points at it in OpenAI-compatible mode (base URL
//     + model in the body), the same path a non-Azure operator uses, so no translation shim is needed.
//   • A mock OIDC issuer (discovery + JWKS) that signs the per-request access token loftd validates.
import net from "node:net";
import http from "node:http";
import crypto from "node:crypto";
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { WebSocket } from "ws";

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = resolve(HERE, "..");
const FIXTURES = join(HERE, "llmock.fixtures.yaml");
const TAG = `loft-test-${process.pid}`;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const freePort = () =>
  new Promise((res, rej) => {
    const s = net.createServer();
    s.once("error", rej);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => res(port));
    });
  });

function docker(args, opts = {}) {
  return execFileSync("docker", args, { encoding: "utf8", ...opts });
}

// --- Postgres: ephemeral container + a restricted, table-owning role (so RLS actually binds) ---
async function startPostgres() {
  const name = `${TAG}-pg`;
  docker(["run", "-d", "--rm", "--name", name, "-e", "POSTGRES_PASSWORD=postgres", "-e", "POSTGRES_DB=loft", "-p", "127.0.0.1::5432", "postgres:16-alpine"], { stdio: "ignore" });
  const portLine = docker(["port", name, "5432/tcp"]).split("\n")[0].trim(); // "127.0.0.1:49xxx"
  const port = portLine.slice(portLine.lastIndexOf(":") + 1);

  // Wait on a real query against the loft DB, because pg_isready can pass during the image's init restart
  // (it starts once for init, then restarts to listen), which races role creation under load.
  let ready = false;
  for (let i = 0; i < 120 && !ready; i++) {
    try { docker(["exec", name, "psql", "-U", "postgres", "-d", "loft", "-tAc", "select 1"], { stdio: "ignore" }); ready = true; }
    catch { await sleep(500); }
  }
  if (!ready) throw new Error("postgres did not become ready");

  // loft_app owns the tables but is NOT superuser and does NOT bypass RLS, like the prod MI role.
  // Idempotent + retried so a transient connection blip during startup doesn't fail the run.
  const roleSQL =
    "do $$ begin if not exists (select from pg_roles where rolname='loft_app') then " +
    "create role loft_app login password 'app' nosuperuser nobypassrls; end if; end $$; " +
    "grant all on schema public to loft_app;";
  let made = false;
  for (let i = 0; i < 10 && !made; i++) {
    try { docker(["exec", name, "psql", "-U", "postgres", "-d", "loft", "-v", "ON_ERROR_STOP=1", "-c", roleSQL], { stdio: "ignore" }); made = true; }
    catch { await sleep(500); }
  }
  if (!made) throw new Error("could not create loft_app role");

  return { name, connectionString: `postgres://loft_app:app@127.0.0.1:${port}/loft` };
}

// --- llmock: the LLM emulator (deterministic, zero latency for a fast suite) ---
async function startLlmock() {
  const name = `${TAG}-llmock`;
  docker(["run", "-d", "--rm", "--name", name,
    "-e", "LLMOCK_DETERMINISTIC=true", "-e", "LLMOCK_TTFT_MS=0", "-e", "LLMOCK_INTER_TOKEN_MS=0",
    "-p", "127.0.0.1::8080", "-v", `${FIXTURES}:/fixtures.yaml:ro`,
    "ghcr.io/larsakerlund/llmock:latest", "--fixtures", "/fixtures.yaml"], { stdio: "ignore" });
  const portLine = docker(["port", name, "8080/tcp"]).split("\n")[0].trim();
  const port = portLine.slice(portLine.lastIndexOf(":") + 1);
  const base = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 60; i++) {
    try { if ((await fetch(`${base}/healthz`)).ok) break; } catch { /* not up yet */ }
    await sleep(250);
    if (i === 59) throw new Error("llmock did not become ready");
  }
  return { name, base };
}

// --- mock OIDC issuer: discovery + JWKS + an RS256 token minter ---
// loftd validates a real Bearer (the only identity path now that the proxy-header fallback is gone),
// so the suite stands up a tiny IdP, points loftd at it, and signs a per-request access token.
function b64url(buf) {
  return Buffer.from(buf).toString("base64").replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function startIdP() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("rsa", { modulusLength: 2048 });
  const kid = "loft-test-1";
  const jwk = { ...publicKey.export({ format: "jwk" }), kid, use: "sig", alg: "RS256" };
  const audience = "loft-api";
  const scope = "access_as_user";
  let issuer = "";
  const server = http.createServer((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url.startsWith("/.well-known/openid-configuration")) {
      res.end(JSON.stringify({
        issuer,
        jwks_uri: `${issuer}/jwks`,
        authorization_endpoint: `${issuer}/authorize`,
        token_endpoint: `${issuer}/token`,
        response_types_supported: ["code"],
        subject_types_supported: ["public"],
        id_token_signing_alg_values_supported: ["RS256"],
      }));
    } else if (req.url.startsWith("/jwks")) {
      res.end(JSON.stringify({ keys: [jwk] }));
    } else {
      res.writeHead(404).end();
    }
  });
  const port = await new Promise((r) => server.listen(0, "127.0.0.1", () => r(server.address().port)));
  issuer = `http://127.0.0.1:${port}`;
  const mint = (email, id, name) => {
    const now = Math.floor(Date.now() / 1000);
    const head = b64url(JSON.stringify({ alg: "RS256", typ: "JWT", kid }));
    const body = b64url(JSON.stringify({ iss: issuer, aud: audience, exp: now + 3600, iat: now, oid: id, email, name, scp: scope }));
    const sig = b64url(crypto.sign("RSA-SHA256", Buffer.from(`${head}.${body}`), privateKey));
    return `${head}.${body}.${sig}`;
  };
  return { issuer, audience, scope, mint, close: () => server.close() };
}

// --- the whole stack ---
export async function setup() {
  const cleanups = [];
  const idp = await startIdP();
  cleanups.push(() => idp.close());
  try {
    const pg = await startPostgres();
    cleanups.push(() => { try { docker(["rm", "-f", pg.name], { stdio: "ignore" }); } catch { /* gone */ } });

    const llmock = await startLlmock();
    cleanups.push(() => { try { docker(["rm", "-f", llmock.name], { stdio: "ignore" }); } catch { /* gone */ } });

    const uploadsDir = mkdtempSync(join(tmpdir(), "loft-uploads-"));
    cleanups.push(() => rmSync(uploadsDir, { recursive: true, force: true }));
    const sitesDir = mkdtempSync(join(tmpdir(), "loft-sites-"));
    cleanups.push(() => rmSync(sitesDir, { recursive: true, force: true }));

    // Backend under test: a prebuilt command if supplied (LOFT_BACKEND_CMD), else build loftd.
    let cmdParts = process.env.LOFT_BACKEND_CMD?.split(" ");
    if (!cmdParts) {
      const bin = join(REPO, "bin", "loftd");
      execFileSync("go", ["build", "-o", bin, "./cmd/loftd"], { cwd: REPO, stdio: "inherit" });
      cmdParts = [bin];
    }

    const port = await freePort();
    const env = {
      ...process.env,
      LOFT_LISTEN: `127.0.0.1:${port}`,
      LOFT_PG_CONNECTION_STRING: pg.connectionString,
      LOFT_UPLOADS_DIR: uploadsDir,
      LOFT_SITES_DIR: sitesDir,
      // OpenAI-compatible mode: loftd talks to llmock directly (model in the body), no Azure shim.
      LOFT_AI_ENDPOINT: `${llmock.base}/openai/v1/`,
      LOFT_AI_MODEL: "gpt-5-mini",
      LOFT_AI_KEY: "test",
      LOFT_CLI_CLIENT_ID: "cli-public-id", // advertised at /.well-known/loft for `loft login`
      LOFT_CLI_SCOPE: "openid offline_access api://loft/access_as_user",
      // Real OIDC validation against the mock IdP: every request carries a signed Bearer.
      LOFT_OIDC_ISSUER: idp.issuer,
      LOFT_API_AUDIENCE: idp.audience,
    };
    const [cmd, ...args] = cmdParts;
    const child = spawn(cmd, args, { env, stdio: ["ignore", "pipe", "pipe"] });
    let log = "";
    child.stdout.on("data", (d) => (log += d));
    child.stderr.on("data", (d) => (log += d));
    cleanups.push(() => { try { child.kill("SIGKILL"); } catch { /* gone */ } });

    const baseUrl = `http://127.0.0.1:${port}`;
    const ctx = makeContext(baseUrl, port, uploadsDir, idp);

    // Ready when the server answers AND the schema migration has run (a db list succeeds).
    for (let i = 0; i < 80; i++) {
      if (child.exitCode !== null) throw new Error(`backend exited (${child.exitCode}):\n${log}`);
      try {
        const r = await ctx.req("GET", "/api/db/__ready", { site: "warmup" });
        if (r.status === 200) break;
      } catch { /* not listening yet */ }
      await sleep(250);
      if (i === 79) throw new Error(`backend not ready:\n${log}`);
    }

    ctx.stop = async () => { for (const c of cleanups.reverse()) await c(); };
    ctx.logs = () => log;
    return ctx;
  } catch (e) {
    for (const c of cleanups.reverse()) { try { await c(); } catch { /* best effort */ } }
    throw e;
  }
}

// HTTP/WS helpers that inject the proxy's identity + tenant headers. site → X-Loft-Site (the trusted
// tenant header nginx sets from the validated server name); user → the X-Forwarded-User id (the
// immutable identity key). Tests run with no OIDC issuer configured, so loftd honors these fallbacks.
function makeContext(baseUrl, port, uploadsDir, idp) {
  const idHeaders = ({ site = "sitea", user = "alice", id, anon = false } = {}) => {
    const h = { "X-Loft-Site": site };
    // The proxy forwards a validated access token; the suite mints one per request (oid = the stable
    // identity key). anon → no token, exercising the 401 paths.
    if (!anon) h["Authorization"] = `Bearer ${idp.mint(`${user}@test`, id ?? user, user)}`;
    return h;
  };

  async function req(method, path, opts = {}) {
    const headers = { ...idHeaders(opts), ...(opts.headers ?? {}) };
    let body;
    if (opts.raw !== undefined) {
      body = opts.raw;
    } else if (opts.body !== undefined) {
      headers["Content-Type"] = headers["Content-Type"] ?? "application/json";
      body = JSON.stringify(opts.body);
    }
    const res = await fetch(`${baseUrl}${path}`, { method, headers, body });
    const text = await res.text();
    let json;
    try { json = JSON.parse(text); } catch { /* not json */ }
    return { status: res.status, text, json, headers: res.headers };
  }

  // Open a WebSocket with identity headers. Returns helpers to await readiness and collect messages.
  function openWs(path, opts = {}) {
    const ws = new WebSocket(`ws://127.0.0.1:${port}${path}`, { headers: idHeaders(opts) });
    const messages = [];
    ws.on("message", (d) => { try { messages.push(JSON.parse(d.toString())); } catch { messages.push(d.toString()); } });
    const ready = new Promise((res, rej) => {
      ws.once("open", res);
      ws.once("error", rej);
      ws.once("unexpected-response", (_req, r) => rej(new Error(`ws rejected: ${r.statusCode}`)));
    });
    return {
      ws,
      messages,
      ready,
      send: (m) => ws.send(typeof m === "string" ? m : JSON.stringify(m)),
      close: () => ws.close(),
    };
  }

  return { baseUrl, uploadsDir, req, openWs };
}
