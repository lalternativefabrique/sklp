//go:build !linux

package devstack

// The embedded dev backend runs services in the space runtime (rootless userns
// + runc), which exists only on Linux. Declining here lets `sklp dev` fall back
// to its docker/process-host path instead of failing outright.

func chooseDevBackend(spaceName string, allowHost bool) (devBackend, bool) { return nil, false }

func embeddedAvailable() bool { return false }

func reapEmbeddedRun(spaceName string) {}
