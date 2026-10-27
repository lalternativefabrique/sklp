#!/usr/bin/env bash
# Build the static CLI binaries the installers download from
# <instance>/dist/<version>/<binary>-<os>-<arch>:
#   sklp  — the developer CLI      (install/sklp.sh,  /install/sklp.sh)
#   sklet — the node-side agent    (install/sklet.sh, /install/sklet.sh)
#           Linux containers, e.g. a downstream image's Dockerfile, so a task agent
#           and a cross-repo GITHUB_TOKEN at every image build)
#
# sklp/sklet are static (CGO_ENABLED=0): no glibc dependency, runs on any
# only consumer today is a Dockerfile FROM a glibc base (debian-slim), never
# a bare host the way sklp/sklet are installed, so musl cross-compilation
# isn't pulled in for a portability need nothing exercises yet. The CLI does
# NOT embed DuckDB (that's core), so CGO=0 is safe for the Go binaries.
# Output goes under dist/ (gitignored) — upload it to whatever serves /dist
# on skalpai.dev.
#
# Usage:
#   scripts/build-cli-binaries.sh [version]      # version defaults to the git short sha
#
# Produces:
#   dist/cli/latest/...                          # copy, for the default install URL
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
# What `sklp version` / `sklet version` print: the same YYYYMMDD-HHMM-<sha>
# shape as the image tags, so a node's binary names the build it came from.
RELEASE="${SKLP_RELEASE:-$(date -u +%Y%m%d-%H%M)-${VERSION}}"
COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo "")"
BUILT="$(date -u +%Y-%m-%d)"
LDFLAGS="-s -w -X main.version=${RELEASE} -X main.commit=${COMMIT} -X main.date=${BUILT}"
OUT_BASE="dist/cli"

# The linux builds embed runc/slirp4netns (//go:embed); fetch the pinned
# assets first so a clean checkout builds.
go run ./space/embed/gen

# name:package pairs — every binary the installers can fetch.
BINARIES="sklp:./cmd/sklp sklet:./cmd/sklet sklp-relay-bridge:./cmd/sklp-relay-bridge"

# Pin the build's source date to HEAD's commit time for reproducible artifacts.
if [ -z "${SOURCE_DATE_EPOCH:-}" ] && git rev-parse HEAD >/dev/null 2>&1; then
  SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)"
  export SOURCE_DATE_EPOCH
fi

# The darwin/arm64 sklp can carry the vz backend, which boots the Linux guest
# on Virtualization.framework instead of shelling out to Lima. That needs cgo
# (Objective-C bindings) and therefore a macOS builder with the Xcode command
# line tools; VZVirtualMachine additionally refuses to start unless the binary
# carries the com.apple.security.virtualization entitlement.
#
# Cross-compiling from Linux still works — it just yields a Lima-only binary,
# because the vz files are behind `darwin && arm64 && cgo`. Set
# SKLP_VZ_BUILD=0 to force that even on macOS.
vz_enabled() {
  [ "$1" = darwin ] && [ "$2" = arm64 ] || return 1
  [ "${SKLP_VZ_BUILD:-1}" != 0 ] || return 1
  [ "$(uname -s)" = Darwin ] || return 1
  command -v xcrun >/dev/null 2>&1 || return 1
  command -v codesign >/dev/null 2>&1
}

VZ_ENTITLEMENTS="${VZ_ENTITLEMENTS:-scripts/vz.entitlements}"

build() {
  name="$1"; pkg="$2"; os="$3"; arch="$4"
  out="${OUT_BASE}/${VERSION}/${name}-${os}-${arch}"
  mkdir -p "$(dirname "$out")"
  cgo=0
  if [ "$name" = sklp ] && vz_enabled "$os" "$arch"; then
    cgo=1
    echo "building ${out} (with the vz backend) ..."
  else
    echo "building ${out} ..."
  fi
  CGO_ENABLED="$cgo" GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags="$LDFLAGS" -o "$out" "$pkg"
  # static-ness sanity (linux/amd64 host only)
  if [ "$os" = linux ] && command -v file >/dev/null; then
    file "$out" | grep -q 'statically linked' || { echo "ERROR: $out is not static"; exit 1; }
  fi
  if [ "$cgo" = 1 ]; then
    # Ad-hoc signing is enough for the entitlement to take effect locally. A
    # binary served from the dist bucket also needs Developer ID signing +
    # notarisation, or Gatekeeper blocks it on download.
    codesign --force --sign "${VZ_SIGN_IDENTITY:--}" \
      --entitlements "$VZ_ENTITLEMENTS" "$out" >/dev/null
    codesign -d --entitlements - "$out" 2>&1 | grep -q virtualization || {
      echo "ERROR: $out is missing the virtualization entitlement"; exit 1; }
  fi
}

for entry in $BINARIES; do
  name="${entry%%:*}"; pkg="${entry#*:}"
  build "$name" "$pkg" linux amd64
  build "$name" "$pkg" linux arm64
done


# sklp also ships for macOS: the CLI runs natively and relays the container
# verbs into a Linux VM (internal/limarelay) — its own on Virtualization.
# framework when built on a Mac (see vz_enabled), otherwise Lima or OrbStack,
# which download the matching sklp-linux-<arch> from this same dist layout.
# sklet stays linux-only.
build sklp ./cmd/sklp darwin arm64
build sklp ./cmd/sklp darwin amd64

# `sklet upgrade` refuses a binary that does not match this list.
(cd "${OUT_BASE}/${VERSION}" && rm -f SHA256SUMS && sha256sum -- * > SHA256SUMS)

# Mirror this version under latest/ so the default installer URL resolves.
rm -rf "${OUT_BASE}/latest"
mkdir -p "${OUT_BASE}/latest"
cp "${OUT_BASE}/${VERSION}/"* "${OUT_BASE}/latest/"

echo ""
echo "Built ${VERSION} under ${OUT_BASE}/:"
ls -lh "${OUT_BASE}/${VERSION}/"

# Upload to the public dist bucket. Mirrors both the versioned path and latest/
# so /install/sklp.sh resolves. Only runs when SKLP_DIST_S3_UPLOAD is set (a
# local-only build leaves it empty and skips). When it IS set, a missing `aws`
# is a HARD failure: a publish that silently uploads nothing but exits 0 is
# worse than a red run — it looks published when the bucket is untouched.
#
# --acl public-read: the installers and every node fetch /cli/ anonymously, so
# each uploaded object must be world-readable. Without it the bucket answers 403
# and a swap on a node cannot download its binary.
DIST_BUCKET="${SKLP_DIST_BUCKET:-skalpai-dist}"
DIST_ENDPOINT="${SKLP_DIST_S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
if [ -n "${SKLP_DIST_S3_UPLOAD:-}" ]; then
  command -v aws >/dev/null || {
    echo "ERROR: SKLP_DIST_S3_UPLOAD set but 'aws' is not on PATH — nothing was uploaded" >&2
    exit 1
  }
  echo ""
  echo "uploading to s3://${DIST_BUCKET}/cli/ ..."
  aws --endpoint-url "$DIST_ENDPOINT" s3 cp --recursive --acl public-read \
    "${OUT_BASE}/${VERSION}/" "s3://${DIST_BUCKET}/cli/${VERSION}/"
  aws --endpoint-url "$DIST_ENDPOINT" s3 cp --recursive --acl public-read \
    "${OUT_BASE}/latest/" "s3://${DIST_BUCKET}/cli/latest/"
  echo "uploaded."
fi

echo ""
echo "Installers resolve from the dist bucket:"
for entry in $BINARIES; do
  name="${entry%%:*}"
  echo "  https://${DIST_BUCKET}.s3.fr-par.scw.cloud/cli/latest/${name}-linux-amd64"
done
echo "  https://${DIST_BUCKET}.s3.fr-par.scw.cloud/cli/latest/skt-linux-amd64"
