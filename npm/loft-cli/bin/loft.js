#!/usr/bin/env node
// Launcher for the loft CLI. The actual binary ships in a per-platform optionalDependency
// (loft-cli-<platform>-<arch>); npm installs only the one matching this machine. We resolve that
// package and exec its binary, passing through args, stdio, and the exit code. No postinstall script
// and no download: the executable bit is preserved through the package tarball.
"use strict";

const { spawnSync } = require("node:child_process");
const path = require("node:path");

const platform = process.platform; // darwin | linux | win32
const arch = process.arch; // arm64 | x64
const pkg = `@larsakerlund/loft-cli-${platform}-${arch}`;
const exe = platform === "win32" ? "loft.exe" : "loft";

let binary;
try {
  binary = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), "bin", exe);
} catch {
  console.error(
    `loft-cli: no prebuilt binary for ${platform}-${arch}.\n` +
      `The optional dependency ${pkg} did not install. Reinstall loft-cli, or build from source\n` +
      `(https://github.com/larsakerlund/loft).`,
  );
  process.exit(1);
}

const result = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (result.error) {
  console.error(`loft-cli: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
