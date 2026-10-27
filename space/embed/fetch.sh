#!/usr/bin/env bash
# Thin wrapper around the Go generator. Downloads + checksum-verifies the
# pinned runc / slirp4netns binaries into this dir for the embedded_runtime
# build. Equivalent to `go generate ./space/embed/`.
set -euo pipefail
cd "$(dirname "$0")"
exec go run ./gen
