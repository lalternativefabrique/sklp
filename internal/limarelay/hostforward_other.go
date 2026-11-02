//go:build !(darwin && arm64 && cgo)

package limarelay

// ForwardHostPorts publishes nothing here: Lima and OrbStack forward guest
// ports themselves, and on Linux the stack binds the host directly.
func ForwardHostPorts(_ []int) (release func()) { return func() {} }
