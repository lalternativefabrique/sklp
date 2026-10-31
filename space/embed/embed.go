package embed

import (
	stdembed "embed"
	"runtime"
)

// bin/ holds the runc and slirp4netns helpers fetched by `go generate
// ./space/embed/`. They are gitignored; only bin/placeholder is committed so
// the package compiles without them and extract fails at runtime instead.
//
//go:embed bin
var binFS stdembed.FS

func asset(name string) []byte {
	b, err := binFS.ReadFile("bin/" + name)
	if err != nil {
		return nil
	}
	return b
}

// ExtractRunc writes the embedded runc into the runtime bin cache and returns
// its absolute path. Idempotent across runs (see extract).
func ExtractRunc(cacheRoot string) (string, error) {
	name := "runc-" + runtime.GOARCH
	return extract(cacheRoot, "runc", assetSHA256(name), asset(name))
}

// ExtractSlirp writes the embedded slirp4netns into the runtime bin cache and
// returns its absolute path. Idempotent across runs (see extract).
func ExtractSlirp(cacheRoot string) (string, error) {
	name := "slirp4netns-" + runtime.GOARCH
	return extract(cacheRoot, "slirp4netns", assetSHA256(name), asset(name))
}

func assetSHA256(name string) string {
	for _, a := range Assets {
		if a.Name == name {
			return a.SHA256
		}
	}
	return ""
}

// HelpersEmbedded reports whether this binary carries its runc and slirp4netns,
// which a plain `go build` without `go generate` silently leaves out.
func HelpersEmbedded() bool {
	return len(asset("runc-"+runtime.GOARCH)) > 0 && len(asset("slirp4netns-"+runtime.GOARCH)) > 0
}
