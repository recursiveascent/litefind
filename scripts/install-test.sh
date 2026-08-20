#!/bin/sh
# Smoke-test install.sh against a local snapshot release via file:// URLs.
set -eu

cd "$(dirname "$0")/.."

cleanup() {
    [ -n "${TMP:-}" ] && rm -rf "$TMP"
}
trap cleanup EXIT

TMP=$(mktemp -d)
BINDIR="$TMP/bin"
mkdir -p "$BINDIR"

# Build a snapshot release so dist/ has fresh archives + checksums.txt.
nix develop -c goreleaser release --clean --snapshot --skip=publish >/dev/null

# Run the installer against the local dist/ dir via file://, forcing the
# install dir so /usr/local/bin writability doesn't affect the test.
echo "Running install.sh against file://$(pwd)/dist ..."
LITEFIND_INSTALL_BASE="file://$(pwd)/dist" \
    LITEFIND_INSTALL_DIR="$BINDIR" \
    sh install.sh

# Assert the binary works.
"$BINDIR/litefind" --version
echo "install-test: OK"
