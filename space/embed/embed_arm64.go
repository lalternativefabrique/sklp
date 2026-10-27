//go:build arm64

package embed

import _ "embed"

const archSuffix = "arm64"

//go:embed runc-arm64
var runcBinary []byte

//go:embed slirp4netns-arm64
var slirpBinary []byte
