//go:build amd64

package embed

import _ "embed"

const archSuffix = "amd64"

//go:embed runc-amd64
var runcBinary []byte

//go:embed slirp4netns-amd64
var slirpBinary []byte
