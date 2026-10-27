// Package meta holds the pinned OCI helper-binary descriptors (runc,
// slirp4netns) WITHOUT a //go:embed directive.
//
// It exists to break a bootstrap cycle: the gen tool needs the asset list to
// download the binaries, but if it imported the parent `embed` package (which
// carries the //go:embed directives) Go would try to COMPILE that package
// first — and the compile fails with "pattern runc: no matching files found"
// precisely when the binaries are absent, i.e. exactly when gen must run. By
// reading the descriptors from this embed-free package, gen can fetch the
// binaries from a clean checkout. The embed package imports meta too, so the
// pins stay single-sourced.
package meta

// Pinned helper versions, per linux arch. Each arch's pair is embedded by its
// GOARCH-tagged embed file; gen fetches every asset so one checkout can
// cross-build both arches.
const (
	RuncVersion  = "1.4.2"
	SlirpVersion = "1.3.4"
)

// Asset describes one downloadable helper binary: its release URL, the local
// filename it is embedded under, and its pinned sha256 (published in the
// release's checksum file).
type Asset struct {
	Name   string // local filename in the embed dir (the //go:embed target)
	URL    string
	SHA256 string
}

// Assets are the helper binaries the gen tool downloads and the per-arch
// embed files embed.
var Assets = []Asset{
	{
		Name:   "runc-amd64",
		URL:    "https://github.com/opencontainers/runc/releases/download/v" + RuncVersion + "/runc.amd64",
		SHA256: "ac8a90f9e225bb9322189937b230cdc5478d5753f0e31e1bda98a5cf06bd9539",
	},
	{
		Name:   "runc-arm64",
		URL:    "https://github.com/opencontainers/runc/releases/download/v" + RuncVersion + "/runc.arm64",
		SHA256: "ea54032310588e115633aa2f4bba8bf9500257f657e1deca88df5778775138db",
	},
	{
		Name:   "slirp4netns-amd64",
		URL:    "https://github.com/rootless-containers/slirp4netns/releases/download/v" + SlirpVersion + "/slirp4netns-x86_64",
		SHA256: "e8d0440de8d8c87072138883bc27cfa02f8b0e8a504badbf335c41f794788cc2",
	},
	{
		Name:   "slirp4netns-arm64",
		URL:    "https://github.com/rootless-containers/slirp4netns/releases/download/v" + SlirpVersion + "/slirp4netns-aarch64",
		SHA256: "fbd8a9cabc716dc53e7c5a00bc7b3e91dbe0eab6b40e6d606b1b34c2ce80cfc0",
	},
}
