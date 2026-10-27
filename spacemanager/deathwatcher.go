package spacemanager

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DeathWatcher abstracts the mechanism by which the agent learns that a managed
// instance died. The real implementation uses pidfd + poll(); the fake uses
// channels driven by the test via SimulateDeath. This seam is what lets the
// reconciliation core be tested without spawning real processes — the same
// pattern K8s uses by abstracting the API client.
type DeathWatcher interface {
	// Watch registers pid under id for death notification. Returns an error if
	// the OS rejects the watch (e.g. pidfd_open fails); the caller schedules a
	// backed-off retry.
	Watch(id string, pid int) error

	// Unwatch removes id from the watch set. Idempotent; called on teardown and
	// when a retired/removed instance no longer needs to be watched.
	Unwatch(id string)

	// Deaths returns a channel that receives instance ids as they die. The
	// channel is never closed; the consumer loops until ctx is cancelled.
	Deaths() <-chan string

	// ExitCode returns the reaped exit code for pid. Reads without consuming, so
	// a re-deferred death can read the same code again; the entry is released by
	// dropReaped once the death is settled. Returns (0, false) when the code is
	// not yet available.
	ExitCode(pid int) (code int, ok bool)

	// ReapNow harvests pid directly without blocking. Used on the job death
	// path when the async reaper may not have caught up yet.
	ReapNow(pid int) (code int, ok bool)

	// requeueDeath re-delivers id on the Deaths channel after a short delay, so
	// onDeath can retry harvesting an exit code the async reaper had not yet
	// stored. Async (own goroutine): never blocks the caller's lock. pid is
	// re-armed so the next onDeath looks up the same dead process.
	requeueDeath(id string, pid int)

	// Wake nudges the watcher out of any blocking wait so the reconcile loop
	// runs immediately (replaces the eventfd write in the old poll loop).
	Wake()

	// Close tears down the watcher. Called once when the agent shuts down.
	Close()
}

// reapRegistry holds the exit code of every child this PROCESS reaped, shared
// by all watchers. It is process-global on purpose: a node agent runs one
// Agent (and thus one pidfdWatcher, each a SIGCHLD subreaper with a Wait4(-1)
// drain) PER SPACE in the same process, so any watcher can reap ANY watcher's
// child — whoever's reapLoop wins the SIGCHLD race harvests the exit. With
// per-watcher maps the code landed in the winner's map while the owner polled
// its own forever: its oneshot sat in "running" with the process long gone and
// its requeueDeath loop spun at full CPU (found live: a fast-exiting migrate on
// a 7-space node, stuck until a manual restart).
//
// Entries are released by dropReaped once the owner has settled the death, not
// on read: onDeath asks twice per death and retries when neither answers. Codes
// nobody asks for (reaped runc helpers, retired instances) are swept once the
// registry grows past reapSweepLen and the entry is older than reapSweepAge.
var reapRegistry = struct {
	mu    sync.Mutex
	codes map[int]reapedExit
}{codes: map[int]reapedExit{}}

type reapedExit struct {
	code int
	at   time.Time
}

const (
	reapSweepLen = 1024
	reapSweepAge = time.Minute
)

func storeReaped(pid, code int) {
	reapRegistry.mu.Lock()
	defer reapRegistry.mu.Unlock()
	reapRegistry.codes[pid] = reapedExit{code: code, at: time.Now()}
	if len(reapRegistry.codes) > reapSweepLen {
		cutoff := time.Now().Add(-reapSweepAge)
		for p, e := range reapRegistry.codes {
			if e.at.Before(cutoff) {
				delete(reapRegistry.codes, p)
			}
		}
	}
}

// takeReaped reads a stored exit code WITHOUT consuming it, so a death that has
// to be retried can read the same code again.
//
// It used to delete on read. onDeath asks twice on one death — ExitCode, then
// ReapNow — and re-defers the death when neither answers; deleting on the first
// read meant a code observed once could be gone by the retry, leaving a oneshot
// that had cleanly exited 0 stuck outside Done until reconcile relaunched it.
// That was the residue of the flaky double-launch left after the shared reap
// registry fixed the cross-watcher theft.
//
// Entries are dropped by dropReaped once the death is settled, and the sweep in
// storeReaped still bounds codes nobody ever claims.
func takeReaped(pid int) (int, bool) {
	reapRegistry.mu.Lock()
	defer reapRegistry.mu.Unlock()
	e, ok := reapRegistry.codes[pid]
	return e.code, ok
}

// dropReaped forgets a pid's exit code once its death is fully handled.
func dropReaped(pid int) {
	reapRegistry.mu.Lock()
	defer reapRegistry.mu.Unlock()
	delete(reapRegistry.codes, pid)
}

// pidfdWatcher is the production DeathWatcher: one pidfd per watched instance,
// multiplexed in a single poll() alongside an eventfd wake trigger. It also
// owns the SIGCHLD subreaper so orphaned container inits are reaped here.
type pidfdWatcher struct {
	logger *log.Logger

	mu   sync.Mutex
	fds  map[string]int // id -> pidfd
	pids map[string]int // id -> pid (for reap lookup)

	wakeFD  int
	deathCh chan string

	sigCh chan os.Signal
	once  sync.Once // guards Close
	done  chan struct{}
	wg    sync.WaitGroup
}

func newPidfdWatcher(logger *log.Logger) *pidfdWatcher {
	w := &pidfdWatcher{
		logger:  logger,
		fds:     make(map[string]int),
		pids:    make(map[string]int),
		deathCh: make(chan string, 64),
		sigCh:   make(chan os.Signal, 1),
		done:    make(chan struct{}),
		wakeFD:  -1,
	}
	return w
}

// Start initialises the eventfd and launches the background goroutines. Must be
// called before Watch/Wake/Deaths. Separated from construction so Run() can
// handle the error path.
func (w *pidfdWatcher) Start(ctx context.Context) error {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		w.logger.Printf("warning: PR_SET_CHILD_SUBREAPER failed: %v (orphan reaping degraded)", err)
	}

	wfd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return err
	}
	w.wakeFD = wfd

	signal.Notify(w.sigCh, unix.SIGCHLD)

	w.wg.Add(2)
	go w.reapLoop(ctx)
	go w.pollLoop(ctx)
	return nil
}

func (w *pidfdWatcher) Watch(id string, pid int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.fds[id] = fd
	w.pids[id] = pid
	w.mu.Unlock()
	w.Wake()
	return nil
}

func (w *pidfdWatcher) Unwatch(id string) {
	w.mu.Lock()
	if fd, ok := w.fds[id]; ok {
		unix.Close(fd)
		delete(w.fds, id)
	}
	delete(w.pids, id)
	w.mu.Unlock()
}

// ExitCode returns the reaped exit code for pid, if available. Called by the
// agent's onDeath path. Reads the PROCESS-wide registry:
// the reapLoop that harvested this pid may belong to another space's watcher.
func (w *pidfdWatcher) ExitCode(pid int) (code int, ok bool) {
	return takeReaped(pid)
}

// ReapNow harvests the exit status of pid on the job death path. The SIGCHLD
// reapLoop (drainReap) ALSO reaps every child with Wait4(-1) and stores the code
// in w.reaped — so a direct Wait4(pid) here RACES it: whoever wins reaps the
// zombie and starves the other (the loser sees ECHILD and returns ok=false,
// which the oneshot path wrongly reads as a FAILURE → the flaky double-relaunch
// of a clean exit 0). So do NOT Wait4 directly. Instead poll w.reaped (filled by
// drainReap) for a short window, nudging the reaper each round, then give up.
func (w *pidfdWatcher) ReapNow(pid int) (int, bool) {
	// First: a direct non-blocking reap. If the SIGCHLD drainReap has not run yet
	// and the child is still ours, this harvests it immediately.
	var ws unix.WaitStatus
	if wpid, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil); wpid == pid && err == nil {
		return ws.ExitStatus(), true
	}
	// Otherwise a drainReap likely won the race — possibly ANOTHER watcher's,
	// since every watcher in this process drains with Wait4(-1) — and stored the
	// code in the shared registry. Read it from there — that is the
	// authoritative copy, NOT an ECHILD-losing direct Wait4.
	return takeReaped(pid)
}

// requeueDeath re-delivers id on the Deaths channel after a short delay so
// onDeath can retry once the SIGCHLD reaper has stored the exit code. It nudges
// that reaper first, then re-enqueues from its own goroutine (never blocking the
// caller). The pid is already dead and Unwatch'd, so this only re-signals the
// agent; it does not reopen a pidfd.
func (w *pidfdWatcher) requeueDeath(id string, pid int) {
	go func() {
		w.Wake() // prompt drainReap to harvest the exit code
		time.Sleep(time.Millisecond)
		select {
		case w.deathCh <- id:
		case <-w.done:
		}
	}()
}

func (w *pidfdWatcher) Deaths() <-chan string { return w.deathCh }

// Wake nudges pollLoop out of a blocking poll(2) by making the wake eventfd
// readable. It holds mu for the write: Close() concurrently closes wakeFD and
// resets it to -1, and an unlocked read could pass the >= 0 check with a fd
// that is closed by the time unix.Write runs — writing into whatever unrelated
// file has since been handed that number.
func (w *pidfdWatcher) Wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wakeFD < 0 {
		return
	}
	var one [8]byte
	one[7] = 1
	_, _ = unix.Write(w.wakeFD, one[:])
}

func (w *pidfdWatcher) Close() {
	w.once.Do(func() {
		signal.Stop(w.sigCh)
		close(w.done)
		w.Wake()
		w.wg.Wait()
		w.mu.Lock()
		if w.wakeFD >= 0 {
			unix.Close(w.wakeFD)
			w.wakeFD = -1
		}
		for id, fd := range w.fds {
			unix.Close(fd)
			delete(w.fds, id)
		}
		w.mu.Unlock()
	})
}

func (w *pidfdWatcher) reapLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			w.drainReap()
			return
		case <-ctx.Done():
			w.drainReap()
			return
		case <-w.sigCh:
			w.drainReap()
			w.Wake()
		}
	}
}

func (w *pidfdWatcher) drainReap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		storeReaped(pid, ws.ExitStatus())
	}
}

func (w *pidfdWatcher) pollLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		pfds, ids := w.buildPollSet()
		timeout := -1 // block until a pidfd fires or we are woken

		n, err := unix.Poll(pfds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			w.logger.Printf("pidfdWatcher: poll error: %v", err)
			return
		}
		if n == 0 {
			continue
		}

		// index 0 is always the wake eventfd. Drain via the fd captured in the
		// poll set, not w.wakeFD: reading the field here would race Close().
		if pfds[0].Revents&unix.POLLIN != 0 {
			var buf [8]byte
			_, _ = unix.Read(int(pfds[0].Fd), buf[:])
		}

		// check if done/ctx after draining wake
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		// Guard the send: deathCh is buffered, but once it fills (a scale-down
		// kills several units at once) an unguarded send blocks forever if the
		// agent has stopped reading — and Close() would deadlock on wg.Wait().
		for i, pfd := range pfds[1:] {
			if pfd.Revents&unix.POLLIN != 0 {
				select {
				case w.deathCh <- ids[i]:
				case <-w.done:
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func (w *pidfdWatcher) buildPollSet() ([]unix.PollFd, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	pfds := make([]unix.PollFd, 0, len(w.fds)+1)
	pfds = append(pfds, unix.PollFd{Fd: int32(w.wakeFD), Events: unix.POLLIN})
	ids := make([]string, 0, len(w.fds))
	for id, fd := range w.fds {
		pfds = append(pfds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		ids = append(ids, id)
	}
	return pfds, ids
}
