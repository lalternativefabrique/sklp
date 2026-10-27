//go:build !linux

package space

func cgroupPIDs(string) []int { return nil }

func processTree(root int) []int { return []int{root} }
