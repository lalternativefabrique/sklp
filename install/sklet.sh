#!/bin/sh
# sklet installer — curl -fsSL https://raw.githubusercontent.com/lalternativefabrique/sklp/main/install/sklet.sh | sh
#
# sklet is the NODE-side agent: it supervises the spaces deployed on this
# machine and serves the control API a workstation pilots with `sklp deploy`.
# It deliberately ships without the dev/CI/publish surface — a production node
# has no reason to carry a pipeline engine. Install sklp instead on a
# workstation (/install/sklp.sh).
#
# Downloads the static sklet binary for this host's OS/arch and installs it into
# ~/.local/bin (no root). Override the source with SKLP_DIST_BASE_URL, the
# version with SKLP_VERSION, and the destination with SKLP_INSTALL_DIR.
#
# Served by core at /install/sklet.sh; https://skalpai-dist.s3.fr-par.scw.cloud/cli is substituted with the
# instance's dist origin at request time.
#
# The binary is fully static (CGO_ENABLED=0), so it runs on any Linux without a
# matching glibc. sklet's runtime (rootless spaces) is Linux-only.

set -eu

# --- config (overridable via env) -------------------------------------------
BASE_URL="${SKLP_DIST_BASE_URL:-https://skalpai-dist.s3.fr-par.scw.cloud/cli}"
VERSION="${SKLP_VERSION:-latest}"
INSTALL_DIR="${SKLP_INSTALL_DIR:-$HOME/.local/bin}"

say()  { printf '%s\n' "$*"; }
err()  { printf 'sklet install: %s\n' "$*" >&2; exit 1; }

# --- detect os/arch ---------------------------------------------------------
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux) ;;
  *) err "unsupported OS: $os (sklet runs rootless spaces, which are Linux-only)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) err "unsupported architecture: $arch (supported: amd64, arm64)" ;;
esac

asset="sklet-${os}-${arch}"
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
tmp="$(mktemp "${TMPDIR:-/tmp}/sklet.XXXXXX")"
trap 'rm -f "$tmp"' EXIT INT TERM

say "downloading ${asset} (${VERSION}) from ${BASE_URL} ..."
fetch "$url" "$tmp" || err "download failed: $url"

# Sanity: a static ELF starts with 0x7f 'E' 'L' 'F'. Guards against a 404 HTML
# page being saved as the binary.
magic="$(head -c 4 "$tmp" | od -An -tx1 2>/dev/null | tr -d ' \n')"
case "$magic" in
  7f454c46) ;; # \x7fELF
  *) err "downloaded file is not an ELF binary (got an error page?): $url" ;;
esac

mkdir -p "$INSTALL_DIR"
chmod 0755 "$tmp"
mv -f "$tmp" "$INSTALL_DIR/sklet"
trap - EXIT INT TERM

say "installed: $INSTALL_DIR/sklet"

# --- capabilities -----------------------------------------------------------
# The agent binds :53 for cross-space DNS, which needs cap_net_bind_service.
# Capabilities live on the inode, so every install drops what the previous
# binary carried: without this, <service>.<space>.internal silently stops
# resolving on the next agent restart, and nothing says why.
caps="cap_net_bind_service,cap_net_admin=+ep"
if command -v setcap >/dev/null 2>&1; then
  if setcap "$caps" "$INSTALL_DIR/sklet" 2>/dev/null; then
    say "capabilities set: $caps"
  elif command -v sudo >/dev/null 2>&1 && sudo -n setcap "$caps" "$INSTALL_DIR/sklet" 2>/dev/null; then
    say "capabilities set: $caps"
  else
    say ""
    say "WARNING: could not set capabilities. Cross-space DNS will NOT work until you run:"
    say "  sudo setcap '$caps' $INSTALL_DIR/sklet"
    say "  # then restart the agent"
  fi
else
  say ""
  say "WARNING: setcap not found (install libcap2-bin). Cross-space DNS needs:"
  say "  sudo setcap '$caps' $INSTALL_DIR/sklet"
fi

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
"$INSTALL_DIR/sklet" version 2>/dev/null || true
say ""
say "Next, on this node:"
say "  sudo $INSTALL_DIR/sklet host setup   # one-time: uidmap, subuid, lingering"
say "  sklet agent --listen :6464           # boot an empty node, wait for a remote deploy"
say ""
say "Then, from your workstation (where sklp is installed):"
say "  sklp deploy login --server https://<this-host>:6464 --token <T> --ca-fingerprint <FP>"
say "  sklp deploy ls"
