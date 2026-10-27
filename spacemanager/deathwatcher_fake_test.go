package spacemanager

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// fakeDeathWatcher implements DeathWatcher without pidfds or real processes.
// The test drives death events explicitly via SimulateDeath(id), which feeds
// Deaths() exactly as a real pidfd firing would — triggering onDeath and the
// full reconciliation path in the agent.
type fakeDeathWatcher struct {
	mu      sync.Mutex
	watched map[string]int // id -> pid registered via Watch
	codes   map[int]int    // pid -> exit code (default 0)

	deathCh chan string
}

func newFakeDeathWatcher() *fakeDeathWatcher {
	return &fakeDeathWatcher{
		watched: make(map[string]int),
		codes:   make(map[int]int),
		deathCh: make(chan string, 64),
	}
}

func (f *fakeDeathWatcher) Watch(id string, pid int) error {
	f.mu.Lock()
	f.watched[id] = pid
	f.mu.Unlock()
	return nil
}

func (f *fakeDeathWatcher) Unwatch(id string) {
	f.mu.Lock()
	delete(f.watched, id)
	f.mu.Unlock()
}

func (f *fakeDeathWatcher) Deaths() <-chan string { return f.deathCh }

func (f *fakeDeathWatcher) ExitCode(pid int) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code, ok := f.codes[pid]
	if ok {
		delete(f.codes, pid)
		return code, true
	}
	return 0, true // default: clean exit, always "known"
}

func (f *fakeDeathWatcher) ReapNow(_ int) (int, bool) { return 0, false }
func (f *fakeDeathWatcher) Wake()                     {}
func (f *fakeDeathWatcher) Close()                    {}

// requeueDeath re-delivers id (the fake's ExitCode is always known, so onDeath
// never actually requeues in tests; provided to satisfy the interface).
func (f *fakeDeathWatcher) requeueDeath(id string, _ int) {
	go func() { f.deathCh <- id }()
}

// SimulateDeath causes id to appear dead — triggers the agent's onDeath path
// exactly as a real pidfd POLLIN would.
func (f *fakeDeathWatcher) SimulateDeath(id string) {
	f.deathCh <- id
}

// SetExitCode configures the exit code ExitCode() returns for pid. Call before
// SimulateDeath so the job/service death path sees the right code.
func (f *fakeDeathWatcher) SetExitCode(pid, code int) {
	f.mu.Lock()
	f.codes[pid] = code
	f.mu.Unlock()
}

// IsWatched reports whether id is currently registered.
func (f *fakeDeathWatcher) IsWatched(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.watched[id]
	return ok
}

// PID returns the pid registered for id (0 if not watched).
func (f *fakeDeathWatcher) PID(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watched[id]
}

// fakeLauncher implements Launcher without real processes. It records Start/Stop
// calls and returns monotonically increasing synthetic PIDs. Wire it with a
// fakeDeathWatcher to control the full instance lifecycle from tests.
type fakeLauncher struct {
	mu      sync.Mutex
	running map[string]int    // id -> current synthetic PID
	addrs   map[string]string // id -> proxy address
	starts  map[string]int    // id -> Start call count
	stops   map[string]int    // id -> Stop call count

	nextPID atomic.Int32
}

func newFakeLauncher() *fakeLauncher {
	f := &fakeLauncher{
		running: make(map[string]int),
		addrs:   make(map[string]string),
		starts:  make(map[string]int),
		stops:   make(map[string]int),
	}
	f.nextPID.Store(10000)
	return f
}

func (f *fakeLauncher) Start(_ context.Context, svc Service) (int, error) {
	pid := int(f.nextPID.Add(1))
	f.mu.Lock()
	f.running[svc.Name] = pid
	f.starts[svc.Name]++
	if f.addrs[svc.Name] == "" {
		f.addrs[svc.Name] = fmt.Sprintf("fake://%s", svc.Name)
	}
	f.mu.Unlock()
	return pid, nil
}

func (f *fakeLauncher) Stop(id string) {
	f.mu.Lock()
	delete(f.running, id)
	f.stops[id]++
	f.mu.Unlock()
}

func (f *fakeLauncher) Addr(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[id]
}

func (f *fakeLauncher) SetAddr(id, addr string) {
	f.mu.Lock()
	f.addrs[id] = addr
	f.mu.Unlock()
}

func (f *fakeLauncher) StartCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[id]
}

func (f *fakeLauncher) StopCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops[id]
}

func (f *fakeLauncher) PID(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id]
}
