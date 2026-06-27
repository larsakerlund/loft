#!/usr/bin/env bash
# Cross-compile the loft CLI for every published platform and drop each binary into its package's
# bin/. CI runs this before `npm publish`-ing the wrapper (loft-cli) and the per-platform packages.
# The binaries are gitignored; only the package.json files are tracked.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"

# package suffix : GOOS : GOARCH   (npm os/cpu on the left, Go's on the right)
targets=(
  "darwin-arm64:darwin:arm64"
  "darwin-x64:darwin:amd64"
  "linux-x64:linux:amd64"
  "linux-arm64:linux:arm64"
  "win32-x64:windows:amd64"
)

for t in "${targets[@]}"; do
  suffix="${t%%:*}"; rest="${t#*:}"; goos="${rest%:*}"; goarch="${rest#*:}"
  exe="loft"; [ "$goos" = "windows" ] && exe="loft.exe"
  out="$here/loft-cli-$suffix/bin/$exe"
  echo "building $suffix ($goos/$goarch) -> $out"
  ( cd "$repo" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
      go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/loft )
done
echo "done."
