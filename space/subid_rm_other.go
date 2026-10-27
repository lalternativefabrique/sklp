//go:build !linux

package space

import "os"

// subidRemoveAll is plain os.RemoveAll off Linux (no userns/subid mapping).
var subidRemoveAll = func(path string) error { return os.RemoveAll(path) }
