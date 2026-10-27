package spacemanager

import (
	"bufio"
	"context"
	"sync"
	"time"
)

// LogSink receives one line of a service's container stdout/stderr, tagged with
// the OWNING SERVICE name (not the instance id) so a reporter renders it like
// `docker compose up` — `[db] db up`. nil = container logs are not streamed.
type LogSink func(service, line string)

// logPathProvider is the optional launcher capability the agent type-asserts to
// stream container logs: it maps an instance id to the host-side file the
// launcher captures the container's stdout/stderr into (runtime startDetached
// writes service.log). The exec test launcher does not implement it, so log
// streaming is simply absent there — no behaviour change for tests.
type logPathProvider interface {
	LogPath(id string) string
}

// logStreamer tails the captured log files of running instances and forwards
// each new line to the sink, prefixed by the service. One streamer serves the
// whole agent: start(id) begins a tail goroutine, stop(id) ends it, and on
// failure tail(id) returns the last lines for the summary. It also retains a
// small ring of recent lines per service so a not-ready service can show WHY in
// the final report without re-reading the file after teardown.
type logStreamer struct {
	sink     LogSink
	provider logPathProvider

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // instance id -> tail canceller
	recent  map[string][]string           // service name -> recent lines (ring)
}

// maxRecentLines caps the per-service ring kept for the failure summary.
const maxRecentLines = 10

const maxRunLines = 200

func newLogStreamer(sink LogSink, provider logPathProvider) *logStreamer {
	return &logStreamer{
		sink:     sink,
		provider: provider,
		cancels:  make(map[string]context.CancelFunc),
		recent:   make(map[string][]string),
	}
}

// enabled reports whether streaming can run (a sink and a path-providing
// launcher are both present). A no-op streamer when not.
func (s *logStreamer) enabled() bool { return s != nil && s.sink != nil && s.provider != nil }

// start begins tailing instance id's captured log, attributing its lines to
// svcName. Idempotent: a second start for the same id replaces the first.
func (s *logStreamer) start(parent context.Context, id, svcName string) {
	if !s.enabled() {
		return
	}
	path := s.provider.LogPath(id)
	if path == "" {
		return
	}
	s.mu.Lock()
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancels[id] = cancel
	s.mu.Unlock()

	go s.tailLoop(ctx, path, svcName)
}

// stop ends the tail goroutine for instance id (idempotent).
func (s *logStreamer) stop(id string) {
	if !s.enabled() {
		return
	}
	s.mu.Lock()
	if cancel := s.cancels[id]; cancel != nil {
		cancel()
		delete(s.cancels, id)
	}
	s.mu.Unlock()
}

// stopAll ends every tail goroutine (teardown).
func (s *logStreamer) stopAll() {
	if !s.enabled() {
		return
	}
	s.mu.Lock()
	for id, cancel := range s.cancels {
		cancel()
		delete(s.cancels, id)
	}
	s.mu.Unlock()
}

// recentLines returns the last buffered lines for a service (for the failure
// summary). Safe to call after stopAll.
func (s *logStreamer) recentLines(svcName string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.recent[svcName]...)
}

// runLines returns the captured output of instance id's last run, and only that
// run. It reads the log file rather than the tailed ring for two reasons: the
// tailer polls, so a job that writes and exits inside one tick printed lines
// the ring never saw; and a fast oneshot that exits during Watch reaches the
// death path without the launch path ever arming a tail, so it has no ring
// entry at all. The path comes from the launcher for the same reason — there
// may have been no start() to record it. The runtime opens the file O_TRUNC per
// launch, so its contents are exactly this run's output.
func (s *logStreamer) runLines(id string) []string {
	if s == nil || s.provider == nil {
		return nil
	}
	path := s.provider.LogPath(id)
	if path == "" {
		return nil
	}
	var lines []string
	if err := DumpLog(path, func(line string) { lines = append(lines, line) }); err != nil {
		return nil
	}
	if len(lines) > maxRunLines {
		lines = lines[len(lines)-maxRunLines:]
	}
	return lines
}

// tailLoop follows path from its start, emitting each new line until ctx is
// done. It tolerates the file not existing yet (the container may not have
// written before the first read) by polling, and follows growth like `tail -f`.
func (s *logStreamer) tailLoop(ctx context.Context, path, svcName string) {
	fl := &lineFollower{path: path}
	defer fl.close()
	emit := func(line string) { s.emit(svcName, line) }
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		fl.poll(emit)
		select {
		case <-ctx.Done():
			fl.flush(emit)
			return
		case <-ticker.C:
		}
	}
}

// readNewLines reads every complete line currently available from r, prepending
// any carried-over partial, and calls emit for each whole line (no trailing
// newline). It returns the new trailing partial to carry into the next pass
// and the number of bytes consumed from r.
func readNewLines(r *bufio.Reader, partial string, emit func(string)) (string, int64) {
	var consumed int64
	for {
		chunk, err := r.ReadString('\n')
		consumed += int64(len(chunk))
		partial += chunk
		if len(partial) > 0 && partial[len(partial)-1] == '\n' {
			emit(partial[:len(partial)-1])
			partial = ""
		}
		if err != nil { // io.EOF (caught up) or a read error: stop, keep partial
			return partial, consumed
		}
	}
}

// emit forwards a line to the sink and records it in the per-service ring.
func (s *logStreamer) emit(svcName, line string) {
	s.mu.Lock()
	r := append(s.recent[svcName], line)
	if len(r) > maxRecentLines {
		r = r[len(r)-maxRecentLines:]
	}
	s.recent[svcName] = r
	s.mu.Unlock()
	s.sink(svcName, line)
}
