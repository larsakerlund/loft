// Black-box acceptance suite for loftd: the contract that the SDK and hosted apps rely on. Every
// assertion is over HTTP/WebSocket, so this same file must stay green against the current Node loftd
// AND any future port (e.g. Go): run `LOFT_BACKEND_CMD="../loftd" node --test` to check the port.
//
// Each test uses its own site/user where state would otherwise leak (rate limits, budgets, and the
// trust-on-first-use collection policy are all process-lived), so tests stay independent.
import { before, after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { existsSync } from "node:fs";
import { join } from "node:path";
import { setup } from "./harness.mjs";

let t;
before(async () => { t = await setup(); }, { timeout: 120_000 });
after(async () => { await t?.stop(); });

const waitFor = async (fn, ms = 2000) => {
  const end = Date.now() + ms;
  while (Date.now() < end) { if (fn()) return true; await new Promise((r) => setTimeout(r, 25)); }
  return false;
};
const relOf = (url) => url.replace(/^\/uploads\//, ""); // "<uuid>/<file>"

describe("auth & identity", () => {
  it("rejects unauthenticated /api/* with 401", async () => {
    assert.equal((await t.req("GET", "/api/me", { anon: true })).status, 401);
  });

  it("returns 404 for anything outside /api/", async () => {
    assert.equal((await t.req("GET", "/", {})).status, 404);
    assert.equal((await t.req("GET", "/index.html", {})).status, 404);
  });

  it("/api/me returns {email,name,id} for the signed-in user", async () => {
    const { status, json } = await t.req("GET", "/api/me", { user: "ada", id: "oid-ada" });
    assert.equal(status, 200);
    assert.deepEqual(Object.keys(json).sort(), ["email", "id", "name"]);
    assert.equal(json.id, "oid-ada");
    assert.equal(json.email, "ada@test");
  });

  it("identity id is independent of the (mutable) email — survives a UPN/email change", async () => {
    const a = await t.req("GET", "/api/me", { user: "old@corp", id: "oid-stable" });
    const b = await t.req("GET", "/api/me", { user: "new@corp", id: "oid-stable" });
    assert.equal(a.json.id, "oid-stable");
    assert.equal(b.json.id, "oid-stable"); // same person, new email → same id
  });
});

describe("loft.db — CRUD", () => {
  const site = "dbcrud";
  it("create returns {id, creator, ...doc}; creator is the token id", async () => {
    const { status, json } = await t.req("POST", "/api/db/notes", { site, user: "u1", id: "oid-u1", body: { text: "hi" } });
    assert.equal(status, 200);
    assert.equal(json.text, "hi");
    assert.ok(json.id);
    assert.equal(json.creator, "oid-u1");
  });

  it("get / list / update / delete round-trip", async () => {
    const created = (await t.req("POST", "/api/db/items", { site, body: { n: 1 } })).json;
    const got = await t.req("GET", `/api/db/items/${created.id}`, { site });
    assert.equal(got.json.n, 1);

    const upd = await t.req("PATCH", `/api/db/items/${created.id}`, { site, body: { n: 2, extra: true } });
    assert.equal(upd.json.n, 2);
    assert.equal(upd.json.extra, true);
    assert.equal(upd.json.id, created.id); // patch merges, id stable

    const del = await t.req("DELETE", `/api/db/items/${created.id}`, { site });
    assert.deepEqual(del.json, { ok: true });
    assert.equal((await t.req("GET", `/api/db/items/${created.id}`, { site })).status, 404);
  });

  it("list respects ?limit", async () => {
    for (let i = 0; i < 3; i++) await t.req("POST", "/api/db/many", { site, body: { i } });
    assert.equal((await t.req("GET", "/api/db/many?limit=2", { site })).json.length, 2);
  });

  it("missing doc → 404; non-object body → 400", async () => {
    assert.equal((await t.req("GET", "/api/db/notes/00000000-0000-0000-0000-000000000000", { site })).status, 404);
    assert.equal((await t.req("POST", "/api/db/notes", { site, body: [1, 2, 3] })).status, 400);
  });
});

describe("loft.db — tenant isolation (RLS)", () => {
  it("one site cannot see or touch another site's documents", async () => {
    const a = (await t.req("POST", "/api/db/secret", { site: "tenantA", body: { k: "v" } })).json;

    assert.deepEqual((await t.req("GET", "/api/db/secret", { site: "tenantB" })).json, []); // not listed
    assert.equal((await t.req("GET", `/api/db/secret/${a.id}`, { site: "tenantB" })).status, 404);
    assert.equal((await t.req("PATCH", `/api/db/secret/${a.id}`, { site: "tenantB", body: { k: "x" } })).status, 404);
    assert.equal((await t.req("DELETE", `/api/db/secret/${a.id}`, { site: "tenantB" })).status, 404);

    // …and the doc is untouched for its real owner-site.
    assert.equal((await t.req("GET", `/api/db/secret/${a.id}`, { site: "tenantA" })).json.k, "v");
  });

  it("ignores a spoofed X-Forwarded-Host — tenant comes only from the trusted X-Loft-Site", async () => {
    const a = (await t.req("POST", "/api/db/spoofcheck", { site: "victim", body: { k: "v" } })).json;
    const spoof = { "X-Forwarded-Host": "victim.loft.test", "Host": "victim.loft.test" };
    // An attacker tenant tries to reach victim's data by spoofing the old host header, which must fail.
    assert.deepEqual((await t.req("GET", "/api/db/spoofcheck", { site: "attacker", headers: spoof })).json, []);
    assert.equal((await t.req("GET", `/api/db/spoofcheck/${a.id}`, { site: "attacker", headers: spoof })).status, 404);
  });
});

describe("loft.db — owner-only authorization", () => {
  const site = "ownz";
  it("ownerOnly: only the creator may update/delete; others get 403", async () => {
    const post = (await t.req("POST", "/api/db/posts?ownerOnly=1", { site, user: "alice", id: "oid-alice", body: { c: "mine" } })).json;
    assert.equal(post.creator, "oid-alice");

    assert.equal((await t.req("DELETE", `/api/db/posts/${post.id}`, { site, user: "bob", id: "oid-bob" })).status, 403);
    assert.equal((await t.req("PATCH", `/api/db/posts/${post.id}`, { site, user: "bob", id: "oid-bob", body: { c: "hax" } })).status, 403);

    assert.equal((await t.req("PATCH", `/api/db/posts/${post.id}`, { site, user: "alice", id: "oid-alice", body: { c: "edited" } })).status, 200);
    assert.equal((await t.req("DELETE", `/api/db/posts/${post.id}`, { site, user: "alice", id: "oid-alice" })).status, 200);
  });

  it("shared (default) collection: anyone on the site may update/delete", async () => {
    const doc = (await t.req("POST", "/api/db/wiki", { site, user: "alice", id: "oid-alice", body: { c: "draft" } })).json;
    assert.equal((await t.req("PATCH", `/api/db/wiki/${doc.id}`, { site, user: "bob", id: "oid-bob", body: { c: "improved" } })).status, 200);
    assert.equal((await t.req("DELETE", `/api/db/wiki/${doc.id}`, { site, user: "carol", id: "oid-carol" })).status, 200);
  });

  it("policy is server-authoritative (trust-on-first-use): the first create wins, later flags ignored", async () => {
    // Locked owner-only on first create → a later create WITHOUT the flag stays owner-only.
    const locked = (await t.req("POST", "/api/db/tofu_locked?ownerOnly=1", { site, user: "alice", id: "oid-alice", body: { x: 1 } })).json;
    await t.req("POST", "/api/db/tofu_locked", { site, user: "alice", id: "oid-alice", body: { x: 2 } }); // no flag, must not unlock
    assert.equal((await t.req("DELETE", `/api/db/tofu_locked/${locked.id}`, { site, user: "bob", id: "oid-bob" })).status, 403);

    // Created shared first → a later ownerOnly=1 cannot retroactively lock it.
    const open = (await t.req("POST", "/api/db/tofu_open", { site, user: "alice", id: "oid-alice", body: { x: 1 } })).json;
    await t.req("POST", "/api/db/tofu_open?ownerOnly=1", { site, user: "alice", id: "oid-alice", body: { x: 2 } });
    assert.equal((await t.req("DELETE", `/api/db/tofu_open/${open.id}`, { site, user: "bob", id: "oid-bob" })).status, 200);
  });
});

describe("loft.db — limits", () => {
  it("oversized document (>256KB) is rejected", async () => {
    // Current loftd aborts the connection mid-body (req.destroy → ECONNRESET); a port might instead
    // return a clean 4xx. The contract that matters: an oversized doc is refused, never stored.
    let status = 0;
    try { status = (await t.req("POST", "/api/db/big", { site: "lim", body: { blob: "x".repeat(300 * 1024) } })).status; }
    catch { status = -1; } // connection reset
    assert.ok(status === -1 || status >= 400, `oversized doc must be refused (got ${status})`);
    assert.equal((await t.req("GET", "/api/db/big", { site: "lim" })).json.length, 0); // nothing stored
  });
});

describe("loft.upload", () => {
  const site = "up";
  it("POST returns {url,name,size} and writes the bytes under the site prefix", async () => {
    const res = await t.req("POST", "/api/upload", { site, raw: Buffer.from("hello"), headers: { "X-Loft-Filename": "a.txt", "Content-Type": "text/plain" } });
    assert.equal(res.status, 200);
    assert.equal(res.json.name, "a.txt");
    assert.equal(res.json.size, 5);
    assert.match(res.json.url, /^\/uploads\/[0-9a-f-]{36}\/a\.txt$/);
    assert.ok(existsSync(join(t.uploadsDir, site, relOf(res.json.url))));
  });

  it("sanitizes the filename", async () => {
    const res = await t.req("POST", "/api/upload", { site, raw: Buffer.from("x"), headers: { "X-Loft-Filename": "../../etc/pa ss wd!.txt" } });
    assert.ok(!res.json.name.includes("/"));
    assert.ok(!res.json.name.includes(" "));
  });

  it("delete removes the file and is idempotent", async () => {
    const up = (await t.req("POST", "/api/upload", { site, raw: Buffer.from("bye"), headers: { "X-Loft-Filename": "d.txt" } })).json;
    const abs = join(t.uploadsDir, site, relOf(up.url));
    assert.ok(existsSync(abs));
    assert.equal((await t.req("DELETE", `/api/upload?path=${encodeURIComponent(up.url)}`, { site })).status, 200);
    assert.ok(!existsSync(abs));
    assert.equal((await t.req("DELETE", `/api/upload?path=${encodeURIComponent(up.url)}`, { site })).status, 200); // idempotent
  });

  it("rejects a malformed delete path with 400", async () => {
    assert.equal((await t.req("DELETE", "/api/upload?path=not-a-valid-path", { site })).status, 400);
  });

  it("one site cannot delete another site's upload", async () => {
    const up = (await t.req("POST", "/api/upload", { site: "victim", raw: Buffer.from("keep"), headers: { "X-Loft-Filename": "f.txt" } })).json;
    const abs = join(t.uploadsDir, "victim", relOf(up.url));
    await t.req("DELETE", `/api/upload?path=${encodeURIComponent(up.url)}`, { site: "attacker" }); // operates on attacker's prefix
    assert.ok(existsSync(abs)); // victim's file is untouched
  });
});

describe("loft.ai", () => {
  it("requires messages (400) and rejects oversized prompts (413)", async () => {
    assert.equal((await t.req("POST", "/api/ai/chat", { site: "ai", body: {} })).status, 400);
    const huge = [{ role: "user", content: "x".repeat(20_000) }];
    assert.equal((await t.req("POST", "/api/ai/chat", { site: "ai", body: { messages: huge } })).status, 413);
  });

  it("non-streaming returns a chat.completion object", async () => {
    const res = await t.req("POST", "/api/ai/chat", { site: "ai", body: { messages: [{ role: "user", content: "hi" }] } });
    assert.equal(res.status, 200);
    assert.equal(res.json.object, "chat.completion");
    assert.equal(res.json.choices[0].message.content, "Hello from llmock.");
  });

  it("streaming emits chat.completion.chunk SSE ending in [DONE]", async () => {
    const res = await t.req("POST", "/api/ai/chat", { site: "ai", body: { messages: [{ role: "user", content: "hi" }], stream: true } });
    assert.equal(res.status, 200);
    const lines = res.text.split("\n\n").map((l) => l.trim()).filter((l) => l.startsWith("data:")).map((l) => l.slice(5).trim());
    assert.ok(lines.includes("[DONE]"), "stream must terminate with [DONE]");
    const chunks = lines.filter((l) => l !== "[DONE]").map((l) => JSON.parse(l));
    const text = chunks.map((c) => c.choices?.[0]?.delta?.content ?? "").join("");
    assert.equal(text, "Hello from llmock.");
  });

  it("maps upstream 429→429 and other upstream errors→502", async () => {
    assert.equal((await t.req("POST", "/api/ai/chat", { site: "ai", body: { messages: [{ role: "user", content: "ERR429 please" }] } })).status, 429);
    assert.equal((await t.req("POST", "/api/ai/chat", { site: "ai", body: { messages: [{ role: "user", content: "ERR500 please" }] } })).status, 502);
  });

  it("enforces the per-user request rate limit", async () => {
    const who = { site: "ratelimit", user: "rl", id: "oid-rl" };
    let limited = false;
    for (let i = 0; i < 25; i++) {
      const r = await t.req("POST", "/api/ai/chat", { ...who, body: { messages: [{ role: "user", content: "hi" }] } });
      if (r.status === 429) { limited = true; break; }
    }
    assert.ok(limited, "expected a 429 within 25 rapid requests");
  });

  it("enforces the per-site daily token budget", async () => {
    const site = "budget";
    assert.equal((await t.req("POST", "/api/ai/chat", { site, user: "b1", id: "oid-b1", body: { messages: [{ role: "user", content: "BIGUSAGE" }] } })).status, 200);
    // A different user, same site → blocked by the site budget (not the per-user rate limit).
    assert.equal((await t.req("POST", "/api/ai/chat", { site, user: "b2", id: "oid-b2", body: { messages: [{ role: "user", content: "hi" }] } })).status, 429);
  });
});

describe("CLI discovery (/.well-known/loft)", () => {
  it("serves public OAuth config with no auth required", async () => {
    const res = await t.req("GET", "/.well-known/loft", { anon: true });
    assert.equal(res.status, 200);
    assert.equal(res.json.clientId, "cli-public-id");
    assert.match(res.json.scope, /access_as_user/);
  });
});

describe("deploy + delete", () => {
  const apex = { site: "" }; // empty X-Loft-Site → the request originates from the apex
  const cli = { "X-Loft-Deploy-Client": "cli" };
  const siteForm = (name, { overwrite = false } = {}) => {
    const fd = new FormData();
    fd.append("site", name); // the server requires the site field before the files
    fd.append("files", new Blob(["<h1>hi</h1>"], { type: "text/html" }), "index.html");
    if (overwrite) fd.append("overwrite", "true");
    return fd;
  };

  it("deploys a named site, refuses overwrite without confirm, then deletes it", async () => {
    const dep = await t.req("POST", "/api/deploy", { ...apex, headers: cli, raw: siteForm("blogtest") });
    assert.equal(dep.status, 200);
    assert.equal(dep.json.site, "blogtest");
    assert.equal(dep.json.files, 1);

    const again = await t.req("POST", "/api/deploy", { ...apex, headers: cli, raw: siteForm("blogtest") });
    assert.equal(again.status, 409); // already exists, overwrite not requested

    const over = await t.req("POST", "/api/deploy", { ...apex, headers: cli, raw: siteForm("blogtest", { overwrite: true }) });
    assert.equal(over.status, 200);

    const del = await t.req("DELETE", "/api/deploy?site=blogtest", { ...apex, headers: cli });
    assert.equal(del.status, 200);
    assert.equal(del.json.deleted, true);

    const del2 = await t.req("DELETE", "/api/deploy?site=blogtest", { ...apex, headers: cli });
    assert.equal(del2.status, 404); // already gone
  });

  it("refuses a browser deploy with neither same-origin nor the CLI header (CSRF gate)", async () => {
    const r = await t.req("POST", "/api/deploy", { ...apex, raw: siteForm("nope") });
    assert.equal(r.status, 403);
  });

  it("refuses a deploy that does not originate from the apex", async () => {
    const r = await t.req("POST", "/api/deploy", { site: "hosted", headers: cli, raw: siteForm("x") });
    assert.equal(r.status, 403);
  });

  it("refuses a hosted site emulating the CLI (Sec-Fetch-Site backstop)", async () => {
    // A page on another origin always carries a browser Sec-Fetch-Site it cannot forge or remove, so
    // even if it manages to send the CLI header the deploy is refused.
    for (const sfs of ["cross-site", "same-site"]) {
      const r = await t.req("POST", "/api/deploy", { ...apex, headers: { ...cli, "Sec-Fetch-Site": sfs }, raw: siteForm("x") });
      assert.equal(r.status, 403, `Sec-Fetch-Site: ${sfs} must be refused`);
    }
    // A same-origin browser request (the console) is allowed.
    const ok = await t.req("POST", "/api/deploy", { ...apex, headers: { ...cli, "Sec-Fetch-Site": "same-origin" }, raw: siteForm("sfsok") });
    assert.equal(ok.status, 200);
    await t.req("DELETE", "/api/deploy?site=sfsok", { ...apex, headers: cli });
  });

  it("a deploy-scoped token (the CLI) may whoami + deploy, but not touch data/AI", async () => {
    const dep = { scope: t.deployScope }; // mint a token with the reduced deploy scope
    // Allowed: identity + deploy/remove.
    assert.equal((await t.req("GET", "/api/me", dep)).status, 200);
    const pub = await t.req("POST", "/api/deploy", { ...apex, ...dep, headers: cli, raw: siteForm("scopetest") });
    assert.equal(pub.status, 200);
    assert.equal((await t.req("DELETE", "/api/deploy?site=scopetest", { ...apex, ...dep, headers: cli })).status, 200);
    // Refused (403, wrong scope): the data, AI, upload, and realtime endpoints.
    assert.equal((await t.req("POST", "/api/db/notes", { ...dep, body: { x: 1 } })).status, 403);
    assert.equal((await t.req("GET", "/api/db/notes", dep)).status, 403);
    assert.equal((await t.req("POST", "/api/ai/chat", { ...dep, body: { messages: [{ role: "user", content: "hi" }] } })).status, 403);
    assert.equal((await t.req("POST", "/api/upload", { ...dep, raw: new FormData() })).status, 403);
  });
});

describe("realtime", () => {
  it("loft.db.subscribe receives create/update/delete for its collection", async () => {
    const site = "rtdb";
    const sub = t.openWs("/api/db/subscribe?collection=live", { site });
    await sub.ready;

    const doc = (await t.req("POST", "/api/db/live", { site, body: { v: 1 } })).json;
    assert.ok(await waitFor(() => sub.messages.some((m) => m.op === "create" && m.doc?.v === 1)));

    await t.req("PATCH", `/api/db/live/${doc.id}`, { site, body: { v: 2 } });
    assert.ok(await waitFor(() => sub.messages.some((m) => m.op === "update" && m.doc?.v === 2)));

    await t.req("DELETE", `/api/db/live/${doc.id}`, { site });
    assert.ok(await waitFor(() => sub.messages.some((m) => m.op === "delete" && m.id === doc.id)));
    sub.close();
  });

  it("loft.socket relays a message to other clients but not the sender", async () => {
    const site = "rtsock";
    const a = t.openWs("/api/socket?channel=room", { site, user: "a" });
    const b = t.openWs("/api/socket?channel=room", { site, user: "b" });
    await Promise.all([a.ready, b.ready]);

    a.send({ hello: "world" });
    assert.ok(await waitFor(() => b.messages.some((m) => m.hello === "world")), "peer should receive");
    await new Promise((r) => setTimeout(r, 150));
    assert.ok(!a.messages.some((m) => m.hello === "world"), "sender should NOT receive its own message");
    a.close(); b.close();
  });

  it("realtime is site-scoped and requires auth", async () => {
    // Cross-site: a subscriber on one site never sees another site's writes.
    const sub = t.openWs("/api/db/subscribe?collection=iso", { site: "isoA" });
    await sub.ready;
    await t.req("POST", "/api/db/iso", { site: "isoB", body: { leak: true } });
    await new Promise((r) => setTimeout(r, 200));
    assert.equal(sub.messages.length, 0);
    sub.close();

    // Unauthenticated upgrade is rejected.
    await assert.rejects(t.openWs("/api/db/subscribe?collection=iso", { site: "isoA", anon: true }).ready);
  });
});
