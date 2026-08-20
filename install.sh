#!/bin/sh
# litefind installer — curl | sh
#
#   curl -fsSL https://litefind.dev/install.sh | sh
#
# Downloads the latest release binary for the current platform, verifies it
# against the release checksums manifest, and installs it. No sudo: installs
# to /usr/local/bin if writable by the current user, otherwise ~/.local/bin.
set -eu

owner=recursiveascent
repo=litefind
base="${LITEFIND_INSTALL_BASE:-https://github.com/${owner}/${repo}/releases/latest/download}"

err() { echo "install: $*" >&2; exit 1; }

# --- platform ----------------------------------------------------------------

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$os" in
    darwin|linux) ;;
    *) err "unsupported OS: $os (expected darwin or linux)" ;;
esac

case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) err "unsupported architecture: $arch (expected amd64 or arm64)" ;;
esac

# --- install location --------------------------------------------------------

if [ -n "${LITEFIND_INSTALL_DIR:-}" ]; then
    bindir="$LITEFIND_INSTALL_DIR"
    mkdir -p "$bindir" || err "could not create $bindir"
elif [ -w /usr/local/bin ]; then
    bindir=/usr/local/bin
else
    bindir="${HOME}/.local/bin"
    mkdir -p "$bindir" || err "could not create $bindir"
fi

# --- fetch + verify + install ------------------------------------------------

tmp=$(mktemp -d) || err "could not create temp dir"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading checksums..."
curl -fsSL "${base}/checksums.txt" -o "${tmp}/checksums.txt" \
    || err "could not download checksums.txt"

line=$(grep -E "_${os}_${arch}\.tar\.gz$" "${tmp}/checksums.txt" || true)
[ -n "$line" ] || err "no checksum entry for ${os}/${arch}"

sha=$(echo "$line" | awk '{print $1}')
filename=$(echo "$line" | awk '{print $2}')
# Defensive: goreleaser writes bare filenames, but strip any leading path
# component in case that ever changes.
filename=$(basename "$filename")

echo "Downloading ${filename}..."
curl -fsSL "${base}/${filename}" -o "${tmp}/${filename}" \
    || err "could not download ${filename}"

echo "Verifying checksum..."
if command -v sha256sum >/dev/null 2>&1; then
    actual=$(sha256sum "${tmp}/${filename}" | awk '{print $1}')
else
    actual=$(shasum -a 256 "${tmp}/${filename}" | awk '{print $1}')
fi

[ "$actual" = "$sha" ] || err "checksum mismatch (expected $sha, got $actual)"

tar -xzf "${tmp}/${filename}" -C "$tmp" litefind \
    || err "could not extract binary from archive"

install -m755 "${tmp}/litefind" "${bindir}/litefind" \
    || err "could not install to ${bindir}/litefind"

# --- PATH note ---------------------------------------------------------------

case ":${PATH}:" in
    *":${bindir}:"*) ;;
    *)
        echo
        echo "Installed to ${bindir}, which is not on your PATH."
        echo "Add it with:"
        echo "  export PATH=\"${bindir}:\$PATH\""
        echo "Then start a new shell or source your shell config."
        ;;
esac

echo
echo "Installed litefind to ${bindir}/litefind"
"${bindir}/litefind" --version || true
