// CLI guardrail acceptance: the `loft deploy` validations that protect a deploy run entirely
// locally, BEFORE any auth or network, so they're hermetic. These pin the exact rejections a port
// (e.g. Go) must reproduce: empty folder, missing entry point, project cruft, leaked secrets,
// disallowed file types, oversized files. (The happy-path mirror-to-storage needs real Azure Files
// (Azurite doesn't emulate Files) so it stays a live smoke, not part of this hermetic suite.)
import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, truncateSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const REPO = resolve(dirname(fileURLToPath(import.meta.url)), "..");
// CLI under test is swappable via LOFT_CLI_CMD; by default it builds and runs the Go `loft` binary.
const CLI_CMD = process.env.LOFT_CLI_CMD ? process.env.LOFT_CLI_CMD.split(" ") : [join(REPO, "bin", "loft")];

before(() => {
  if (!process.env.LOFT_CLI_CMD) execFileSync("go", ["build", "-o", join(REPO, "bin", "loft"), "./cmd/loft"], { cwd: REPO, stdio: "inherit" });
}, { timeout: 60_000 });

// Write a {relPath: contents} map into a fresh temp dir and return it.
function makeSite(files) {
  const dir = mkdtempSync(join(tmpdir(), "loft-cli-"));
  for (const [rel, content] of Object.entries(files)) {
    const abs = join(dir, rel);
    mkdirSync(dirname(abs), { recursive: true });
    if (content === "@@BIG@@") { writeFileSync(abs, ""); truncateSync(abs, 26 * 1024 * 1024); } // sparse, 26 MB
    else writeFileSync(abs, content);
  }
  return dir;
}

const deploy = (dir, args = []) => {
  const r = spawnSync(CLI_CMD[0], [...CLI_CMD.slice(1), "deploy", dir, ...args], { encoding: "utf8" });
  return { code: r.status, out: (r.stdout ?? "") + (r.stderr ?? "") };
};

describe("loft CLI — deploy guardrails (hermetic, pre-network)", () => {
  it("usage with no command, exit 0", () => {
    const r = spawnSync(CLI_CMD[0], CLI_CMD.slice(1), { encoding: "utf8" });
    assert.equal(r.status, 0);
    assert.match(r.stdout, /deploy/);
  });

  it("empty folder is rejected", () => {
    const r = deploy(makeSite({}));
    assert.equal(r.code, 1);
    assert.match(r.out, /empty/);
  });

  it("missing index.html is rejected", () => {
    const r = deploy(makeSite({ "app.js": "console.log(1)" }));
    assert.equal(r.code, 1);
    assert.match(r.out, /index\.html/);
  });

  it("node_modules/ is rejected", () => {
    const r = deploy(makeSite({ "index.html": "<h1>hi</h1>", "node_modules/dep.js": "x" }));
    assert.equal(r.code, 1);
    assert.match(r.out, /node_modules/);
  });

  it("a leaked .env is refused", () => {
    const r = deploy(makeSite({ "index.html": "<h1>hi</h1>", ".env": "SECRET=1" }));
    assert.equal(r.code, 1);
    assert.match(r.out, /secret/i);
  });

  it("disallowed file types are rejected", () => {
    const r = deploy(makeSite({ "index.html": "<h1>hi</h1>", "tool.exe": "MZ" }));
    assert.equal(r.code, 1);
    assert.match(r.out, /disallowed file types/);
  });

  it("a file over 25 MB is rejected", () => {
    const r = deploy(makeSite({ "index.html": "<h1>hi</h1>", "movie.mp4": "@@BIG@@" }));
    assert.equal(r.code, 1);
    assert.match(r.out, /over 25/);
  });
});
