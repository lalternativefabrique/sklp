# Embedded OCI helper binaries

The embedded runtime (`//go:build embedded_runtime`) shells out to two external
binaries: **runc** (container lifecycle) and **slirp4netns** (rootless egress).
To make a built `sklp` self-contained, these are baked in via `//go:embed` and
extracted to the runtime cache at first use — nothing is required on `$PATH`.

## The binaries are generated build inputs

`runc` and `slirp4netns` are gitignored generated files. They may exist in a
developer workspace after `go generate`, but they are not source artifacts and
must not be committed. The generated files are checksum-verified when fetched
and the embedded bytes are checked again against the pinned hashes at extraction
time, so a stale or locally replaced helper fails loudly instead of being used.

## Regenerate (before any `-tags embedded_runtime` build)

```bash
go generate ./space/embed/   # or: ./space/embed/fetch.sh
```

This runs `gen/main.go`, which downloads each asset in `versions.go` and
verifies it against its pinned sha256 (idempotent — skips files already on
disk with the right hash).

If you build with `-tags embedded_runtime` before running this, the build fails
with `pattern runc: no matching files found`. Run the generate step, then
rebuild.

## Pinned versions

| Binary       | Version  | Asset                  | Arch       |
|--------------|----------|------------------------|------------|
| runc         | v1.4.2   | `runc.amd64`           | amd64 only |
| slirp4netns  | v1.3.4   | `slirp4netns-x86_64`   | amd64 only |

Hashes live in `versions.go`. **amd64 only**: the runner is amd64-only. arm64
support lands with the multi-arch milestone (add per-arch assets + an
arch-tagged embed file).

## Build flow

The runner image and local dev should run `go generate ./space/embed/`
before `go build -tags embedded_runtime`. The default (`go build ./...`, no tag)
does not compile this package's embed file, so the binaries are not required.
