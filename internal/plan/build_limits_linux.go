//go:build linux

package plan

import "github.com/lalternativefabrique/sklp/space/ociproc"

const minBuildCPUs = 2

// buildLimits leaves half of the CPU budget and half of the RAM to the host.
func buildLimits(cpus int, memoryBytes int64) ociproc.Limits {
	cores := cpus / 2
	if cores < minBuildCPUs {
		cores = minBuildCPUs
	}
	if cpus > 0 && cores > cpus {
		cores = cpus
	}
	return ociproc.Limits{
		CPUCores:    float64(cores),
		MemoryBytes: memoryBytes / 2,
		PidsMax:     buildPidsLimit,
	}
}
