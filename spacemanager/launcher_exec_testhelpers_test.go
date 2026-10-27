package spacemanager

// Test-only accessors over execLauncher's pid bookkeeping, used by the
// replica/proxy/rolling tests to find a live instance's PID so they can kill it
// and assert the agent's reaction.

// pidOf returns the last recorded PID for instance id (0 if none).
func (l *execLauncher) pidOf(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pids[id]
}

// pidsOf returns a one-element slice with the current PID for id, or empty when
// the instance has not been launched. (execLauncher keeps only the latest PID
// per id; the slice shape keeps call sites uniform with countingExecLauncher.)
func (l *execLauncher) pidsOf(id string) []int {
	if p := l.pidOf(id); p > 0 {
		return []int{p}
	}
	return nil
}
