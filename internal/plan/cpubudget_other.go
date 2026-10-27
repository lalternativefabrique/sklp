//go:build !linux

package plan

func cgroupCPUs() int { return 0 }
