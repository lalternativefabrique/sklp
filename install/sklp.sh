#!/bin/sh
# sklp installer — curl -fsSL https://raw.githubusercontent.com/lalternativefabrique/sklp/main/install/sklp.sh | sh
#
# Downloads the static sklp binary for this host's OS/arch and installs it into
# ~/.local/bin (no root). Override the source with SKLP_DIST_BASE_URL, the
# version with SKLP_VERSION, and the destination with SKLP_INSTALL_DIR.
# (Not SKLP_BASE_URL — that names the LLM provider endpoint elsewhere.)
#
# Served by core at /install/sklp.sh; https://skalpai-dist.s3.fr-par.scw.cloud/cli is substituted with the
# instance's dist origin at request time.
#
# The binary is fully static (CGO_ENABLED=0), so it runs on any Linux without a
# matching glibc. sklp's runtime (rootless spaces) is Linux-only; on other OSes
# this installs the CLI but `sklp deploy` will report it is Linux-only.

set -eu

# --- config (overridable via env) -------------------------------------------
BASE_URL="${SKLP_DIST_BASE_URL:-https://skalpai-dist.s3.fr-par.scw.cloud/cli}"
VERSION="${SKLP_VERSION:-latest}"
INSTALL_DIR="${SKLP_INSTALL_DIR:-$HOME/.local/bin}"

say()  { printf '%s\n' "$*"; }
err()  { printf 'sklp install: %s\n' "$*" >&2; exit 1; }

case "$BASE_URL" in
  https://*) ;;
  *) err "SKLP_DIST_BASE_URL must be https: $BASE_URL" ;;
esac

# --- detect os/arch ---------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux)  ;;
  darwin) say "note: sklp CLI installs on macOS, but rootless spaces are Linux-only" ;;
  *) err "unsupported OS: $os (sklp targets linux; darwin is CLI-only)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) err "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

asset="sklp-${os}-${arch}"
url="${BASE_URL}/${VERSION}/${asset}"

# --- pick a downloader ------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
else
  err "need curl or wget to download"
fi

# --- download to a temp file, then install atomically ----------------------
tmp="$(mktemp "${TMPDIR:-/tmp}/sklp.XXXXXX")"
trap 'rm -f "$tmp"' EXIT INT TERM

say "downloading ${asset} (${VERSION}) from ${BASE_URL} ..."
fetch "$url" "$tmp" || err "download failed: $url"

sums="$(mktemp "${TMPDIR:-/tmp}/sklp-sums.XXXXXX")"
trap 'rm -f "$tmp" "$sums"' EXIT INT TERM
fetch "${BASE_URL}/${VERSION}/SHA256SUMS" "$sums" || err "download failed: ${BASE_URL}/${VERSION}/SHA256SUMS"
want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$sums")"
[ -n "$want" ] || err "no checksum for $asset in SHA256SUMS"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  got="$(shasum -a 256 "$tmp" | awk '{print $1}')"
else
  err "need sha256sum or shasum to verify the download"
fi
[ "$got" = "$want" ] || err "checksum mismatch for $asset (want $want, got $got)"

# Sanity: a static ELF (linux) starts with 0x7f 'E' 'L' 'F'. Guards against a
# 404 HTML page being saved as the binary.
if [ "$os" = "linux" ]; then
  magic="$(head -c 4 "$tmp" | od -An -tx1 2>/dev/null | tr -d ' \n')"
  case "$magic" in
    7f454c46) ;; # \x7fELF
    *) err "downloaded file is not an ELF binary (got an error page?): $url" ;;
  esac
fi

mkdir -p "$INSTALL_DIR"
chmod 0755 "$tmp"
mv -f "$tmp" "$INSTALL_DIR/sklp"
rm -f "$sums"
trap - EXIT INT TERM

say "installed: $INSTALL_DIR/sklp"

# --- PATH hint --------------------------------------------------------------
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say ""
    say "NOTE: $INSTALL_DIR is not on your PATH. Add it (then re-open your shell):"
    say "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.bashrc"
    ;;
esac

# --- version + next steps ---------------------------------------------------
say ""
"$INSTALL_DIR/sklp" version 2>/dev/null || true
say ""
say "Next, on a Linux host that will run rootless spaces:"
say "  sudo sklp host setup   # one-time: uidmap, subuid, lingering"
