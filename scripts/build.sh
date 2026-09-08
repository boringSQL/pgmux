#!/usr/bin/env bash
# Builds static pgmuxd binaries and their checksums.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
# From the commit, not the clock, so the same source yields the same checksum.
BUILD_DATE="${BUILD_DATE:-$(git log -1 --format=%cI 2>/dev/null || echo unknown)}"

LDFLAGS="-s -w"
LDFLAGS="$LDFLAGS -X main.version=$VERSION"
LDFLAGS="$LDFLAGS -X main.commit=$COMMIT"
LDFLAGS="$LDFLAGS -X main.buildDate=$BUILD_DATE"

rm -rf dist
mkdir -p dist

for arch in amd64 arm64; do
    output="dist/pgmuxd-linux-$arch"
    echo "building $output ($VERSION)"
    # CGO off: static binary, no libc needed in the exec chroot.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$output" ./cmd/pgmuxd
done

# sha256sum is GNU-only; fall back to shasum on macOS.
if command -v sha256sum >/dev/null 2>&1; then
    (cd dist && sha256sum pgmuxd-* > SHA256SUMS)
else
    (cd dist && shasum -a 256 pgmuxd-* > SHA256SUMS)
fi

echo
cat dist/SHA256SUMS
