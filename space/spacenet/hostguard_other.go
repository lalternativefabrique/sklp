//go:build !linux

package spacenet

// SetControllerPorts is a no-op off Linux: there is no space netns to guard.
func SetControllerPorts([]int) {}
