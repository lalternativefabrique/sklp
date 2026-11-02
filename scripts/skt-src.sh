#!/usr/bin/env bash
# Prints a checkout of github.com/lalternativefabrique/skt at the version
# pinned in scripts/skt.pin, cloning or updating it under the user cache.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
ref="$(tr -d '[:space:]' < "$root/scripts/skt.pin")"
[ -n "$ref" ] || { echo "skt-src: scripts/skt.pin is empty" >&2; exit 1; }

dir="${SKT_SRC_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/skalpai/skt-src}"
if [ ! -d "$dir/.git" ]; then
  git clone -q https://github.com/lalternativefabrique/skt.git "$dir"
fi
if ! git -C "$dir" cat-file -e "$ref^{commit}" 2>/dev/null; then
  git -C "$dir" fetch -q --tags origin
fi
git -C "$dir" checkout -q --detach "$ref"
echo "$dir"
