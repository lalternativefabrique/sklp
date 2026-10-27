package embed

// runcBinary / slirpBinary are the helper binaries baked into sklp at build
// time, declared in the GOARCH-tagged embed_<arch>.go files. They are NOT in
// git: `go generate ./apps/sklp/space/embed/` (or ./fetch.sh) downloads +
// checksum-verifies them first. If they are absent the build fails with
// "pattern runc-<arch>: no matching files found" — run the generate step,
// then rebuild with -tags embedded_runtime.

// ExtractRunc writes the embedded runc into the runtime bin cache and returns
// its absolute path. Idempotent across runs (see extract).
func ExtractRunc(cacheRoot string) (string, error) {
	return extract(cacheRoot, "runc", assetSHA256("runc-"+archSuffix), runcBinary)
}

// ExtractSlirp writes the embedded slirp4netns into the runtime bin cache and
// returns its absolute path. Idempotent across runs (see extract).
func ExtractSlirp(cacheRoot string) (string, error) {
	return extract(cacheRoot, "slirp4netns", assetSHA256("slirp4netns-"+archSuffix), slirpBinary)
}

func assetSHA256(name string) string {
	for _, a := range Assets {
		if a.Name == name {
			return a.SHA256
		}
	}
	return ""
}
