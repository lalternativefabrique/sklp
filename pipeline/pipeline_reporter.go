// pipeline_reporter.go wires the pretty reporter (PR #375) into the
// `kind: pipeline` execution path. The deterministic CI executor in
// ci_executor.go already routes its progress through reporter.Reporter
// — this file extends the same UX to the more common pipeline-kind
// path that `sklp run ci` takes on real repositories.
//
// Design notes:
//
//   - Exactly ONE reporter instance per pipeline run. It is created in
//     RunPipeline and threaded into every helper that emits progress
//     (step starts/finishes, scope-gated skips, the final banner).
//
//   - Per-step stdout/stderr is forwarded line-by-line into a writer
//     chain that filters noise (`[no test files]`) and surfaces failure
//     blocks via reporter.Failure. The full unfiltered stream is also
//     mirrored to a per-run log file in the central store
//     (CentralLogDir) so power users can still grep and the cockpit
//     can tail it regardless of cwd.
//
//   - Tool installation and dependency auto-install lines from
//     pipeline_tools_resolve.go are intercepted by toolLogWriter: the
//     useful "installing X@V" lines become dim SubAction bullets, the
//     verbose path / progress lines go to the log file only.
//
//   - Service container logs (sidecar boot output, Postgres / NATS
//     shutdown chatter) are routed to the log file by default. They
//     never appear in the operator's terminal.
package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// pipelineLogFile creates a best-effort per-run log file in the CENTRAL log
// store: <cache>/sklp/logs/<repo-slug>/ci-<task>-<timestamp>.log. The task name
// is in the FILENAME so concurrent pipelines in the same repo (ci-sklp, secops,
// ci-skt…) get distinct logs — keyed by repo alone they'd all collide on "the
// newest log" and the cockpit would tail one log for every space. Centralising
// means the cockpit — a global view from an arbitrary cwd — finds every run's
// log. Errors are non-fatal: io.Discard on failure so callers need no nil check.
func pipelineLogFile(repoPath, task string) (io.WriteCloser, string) {
	logDir := CentralLogDir(repoPath)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nopWriteCloser{io.Discard}, ""
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(logDir, ciLogName(task, ts))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nopWriteCloser{io.Discard}, ""
	}
	return f, path
}

// ciLogName builds a run-log filename: ci-<task>-<ts>.log, or ci-<ts>.log when
// task is empty (a host run with no task name). TaskFilenameSlug keeps the task
// safe and unambiguous as a filename segment.
func ciLogName(task, ts string) string {
	if s := TaskFilenameSlug(task); s != "" {
		return fmt.Sprintf("ci-%s-%s.log", s, ts)
	}
	return fmt.Sprintf("ci-%s.log", ts)
}

// TaskFilenameSlug normalises a task name into a filename-safe slug: lowercase,
// non-[a-z0-9] runs collapsed to "-", edges trimmed. "" stays "".
func TaskFilenameSlug(task string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(task) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// CentralLogDir returns the central per-repo CI log directory:
// <XDG_CACHE_HOME|~/.cache>/sklp/logs/<repo-slug>. repoSlug is the repo's
// basename, keeping the path human-readable while avoiding collisions between
// repos. Both the reporter (writer) and the cockpit (reader) resolve the same
// layout so logs are found independent of cwd.
func CentralLogDir(repoPath string) string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(home, ".cache")
		} else {
			base = filepath.Join(os.TempDir(), "sklp-cache")
		}
	}
	return filepath.Join(base, "sklp", "logs", repoSlug(repoPath))
}

// CentralLogRoot is the parent of all per-repo CI log dirs:
// <XDG_CACHE_HOME|~/.cache>/sklp/logs. The cockpit scans this to find runs
// across every repo, independent of its own cwd.
func CentralLogRoot() string {
	// CentralLogDir("") gives <root>/<unknown>; trim the last element back to
	// the shared root so callers get the parent dir.
	return filepath.Dir(CentralLogDir("x"))
}

// LatestCILogForRepo returns the newest ci-*.log for ONE repo's central dir,
// across all tasks. Used as a repo-wide fallback; prefer LatestCILogForTask.
func LatestCILogForRepo(repoPath string) string {
	return newestCILogIn(CentralLogDir(repoPath))
}

// LatestCILogForTask returns the newest log for ONE task in a repo:
// the newest ci-<task>-*.log in CentralLogDir(repoPath). This is the cockpit's
// exact resolver — a CI space maps to its task (the space name), so each space
// tails its OWN pipeline's log, not the repo's most recent. Returns "" when the
// task has no run log. An empty task falls back to the repo-wide newest.
func LatestCILogForTask(repoPath, task string) string {
	slug := TaskFilenameSlug(task)
	if slug == "" {
		return LatestCILogForRepo(repoPath)
	}
	return newestCILogWithPrefix(CentralLogDir(repoPath), "ci-"+slug+"-")
}

// LatestDevLogForStack returns the newest dev-<stack>-*.log in a repo's central
// dir — the `sklp dev` counterpart of LatestCILogForTask. An empty stack falls
// back to the repo's newest dev log, whatever the stack. Returns "" when none
// exists.
func LatestDevLogForStack(repoPath, stack string) string {
	prefix := "dev-"
	if slug := TaskFilenameSlug(stack); slug != "" {
		prefix += slug + "-"
	}
	return newestCILogWithPrefix(CentralLogDir(repoPath), prefix)
}

// LatestCentralCILog returns the newest ci-*.log across ALL repos under the
// central log root. Fallback for when the repo is unknown; prefer
// LatestCILogForRepo. Returns "" when none exists.
func LatestCentralCILog() string {
	repos, err := os.ReadDir(CentralLogRoot())
	if err != nil {
		return ""
	}
	newestName, newestPath := "", ""
	for _, repo := range repos {
		if !repo.IsDir() {
			continue
		}
		if p := newestCILogIn(filepath.Join(CentralLogRoot(), repo.Name())); p != "" {
			if n := filepath.Base(p); n > newestName {
				newestName, newestPath = n, p
			}
		}
	}
	return newestPath
}

// newestCILogIn returns the newest ci-*.log in dir, across all tasks.
func newestCILogIn(dir string) string {
	return newestCILogWithPrefix(dir, "ci-")
}

// newestCILogWithPrefix returns the newest <prefix>*.log in dir, comparing by
// the trailing YYYYMMDD-HHMMSS timestamp (NOT the whole filename — the task
// segment varies in length and would otherwise dominate the ordering). Returns
// "" when dir has no matching file.
func newestCILogWithPrefix(dir, prefix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	bestTS, bestName := "", ""
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".log") {
			continue
		}
		ts := logTimestamp(n)
		if ts > bestTS {
			bestTS, bestName = ts, n
		}
	}
	if bestName == "" {
		return ""
	}
	return filepath.Join(dir, bestName)
}

// logTimestamp extracts the trailing YYYYMMDD-HHMMSS from a ci log filename
// (ci-<task>-<ts>.log or ci-<ts>.log): drop the .log suffix, take the last two
// dash-separated fields (date-time). Returns "" if the shape is unexpected.
func logTimestamp(name string) string {
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "-" + parts[len(parts)-1]
}

// repoSlug derives a stable, readable directory name from a repo path: its
// basename, with path separators stripped. Empty repoPath yields "unknown".
func repoSlug(repoPath string) string {
	s := filepath.Base(strings.TrimRight(repoPath, string(filepath.Separator)))
	if s == "" || s == "." || s == string(filepath.Separator) {
		return "unknown"
	}
	return s
}

// nopWriteCloser adapts an io.Writer that has no real Close (like
// io.Discard) to the io.WriteCloser interface the caller defers on.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// stepStats tallies the inline summary printed next to a finished
// step: "N passed · M skipped (no tests)". The numbers come from the
// `ok`/`?` go-test prefixes the filter writer observes while streaming
// the step's stdout.
type stepStats struct {
	passed      int
	skippedPkgs int
	failures    []reporter.Failure
}

// summary renders the per-step inline summary. Empty when the step
// produced nothing recognisable (e.g. branch-context, which just runs
// git commands).
func (s *stepStats) summary() string {
	parts := []string{}
	if s.passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", s.passed))
	}
	if s.skippedPkgs > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped (no tests)", s.skippedPkgs))
	}
	if len(s.failures) > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", len(s.failures)))
	}
	return strings.Join(parts, " · ")
}

// goTestOKLine matches the `ok   <pkg>   0.123s` line `go test` emits
// per passing package. Captures the package path so the line writer
// can keep `passed` counters honest without re-implementing gotestsum.
var goTestOKLine = regexp.MustCompile(`^ok\s+\S+\s+\S+`)

// goTestFailLine matches `--- FAIL: TestX (0.00s)` — the canonical
// per-test failure marker. We capture the test name to use as the
// Failure title.
var goTestFailLineRE = regexp.MustCompile(`^---\s+FAIL:\s+(\S+)\s*(?:\(([^)]+)\))?`)

// goTestFileLineRE catches the `    foo_test.go:42:` lines `go test`
// prints right before the failure message. We use the first match as
// the Failure.Location so the FAILURES section shows a click-thru.
var goTestFileLineRE = regexp.MustCompile(`^\s+([\w./-]+\.go:\d+):`)

// stepLineWriter is the per-step io.Writer wired to runShellStep's
// stdout AND stderr. It:
//
//   - Buffers bytes until a newline, then dispatches per-line.
//   - Drops `EMPTY <pkg>` / `?  <pkg>  [no test files]` noise from
//     gotestsum / go-test (filterEmpty == true). Suppressed lines bump
//     stats.skippedPkgs so the final per-step summary stays accurate.
//   - Detects `--- FAIL: TestX` blocks and aggregates them into
//     stats.failures. The full block is still mirrored to logW for
//     the operator's `--debug` grep workflow.
//   - Always mirrors every line (including suppressed ones) to logW
//     so the log file is a faithful capture.
type stepLineWriter struct {
	logW   io.Writer
	liveW  io.Writer // operator's terminal — receives filtered lines (no EMPTY noise)
	stats  *stepStats
	buf    bytes.Buffer
	stderr *bytes.Buffer // tail capture for diagnose
	// failCtx accumulates the body of the current --- FAIL block. We
	// stop collecting at the next `--- FAIL` / `--- PASS` / `=== RUN`
	// boundary, or when we see the package summary line that ends the
	// `go test` output. Bounded to avoid pathological growth.
	failCtx     *strings.Builder
	failTitle   string
	failPending bool

	// tail is a fixed-size ring of the last `tailSize` non-empty lines
	// observed by handleLine. Empty lines and lines suppressed by
	// isEmptyPkgLine are NOT added — they would just dilute the tail
	// with `[no test files]` / EMPTY noise. The runner reads this via
	// Tail() right after Flush() to surface a verdict block below each
	// step's recap, regardless of success or failure (issue
	// d3eba457-04a4-4496-917e-080ea103fdf1: DAST scans previously
	// showed only "✓ passed" with no scan verdict).
	tail    [tailSize]string
	tailLen int // number of valid entries (<= tailSize)
	tailPos int // next write index
}

// tailSize is the number of non-empty trailing lines surfaced under
// each step's recap. Hard-coded — see issue
// d3eba457-04a4-4496-917e-080ea103fdf1.
const tailSize = 5

func newStepLineWriter(logW, liveW io.Writer, stats *stepStats, stderrTail *bytes.Buffer) *stepLineWriter {
	return &stepLineWriter{logW: logW, liveW: liveW, stats: stats, stderr: stderrTail}
}

// Tail returns the last `tailSize` non-empty lines seen by the writer,
// in observation order (oldest first). Callers SHOULD call Flush()
// first so a final no-newline line is included.
func (w *stepLineWriter) Tail() []string {
	if w.tailLen == 0 {
		return nil
	}
	out := make([]string, 0, w.tailLen)
	start := 0
	if w.tailLen == tailSize {
		start = w.tailPos
	}
	for i := 0; i < w.tailLen; i++ {
		out = append(out, w.tail[(start+i)%tailSize])
	}
	return out
}

// pushTail appends line to the ring. Caller must guarantee the line
// is non-empty and not filtered by isEmptyPkgLine.
func (w *stepLineWriter) pushTail(line string) {
	w.tail[w.tailPos] = line
	w.tailPos = (w.tailPos + 1) % tailSize
	if w.tailLen < tailSize {
		w.tailLen++
	}
}

// Write splits incoming bytes into lines and dispatches each to
// handleLine. Partial trailing lines are buffered for the next call.
func (w *stepLineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		raw := w.buf.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := string(raw[:i])
		w.buf.Next(i + 1)
		w.handleLine(line)
	}
	return len(p), nil
}

// Flush drains any buffered trailing line (no newline yet) and closes
// the active FAIL block if one was being accumulated. Always safe to
// call multiple times.
func (w *stepLineWriter) Flush() {
	if w.buf.Len() > 0 {
		w.handleLine(w.buf.String())
		w.buf.Reset()
	}
	w.closeFailBlock()
}

func (w *stepLineWriter) handleLine(line string) {
	// Mirror everything verbatim into the per-run log file. The log
	// file is the unfiltered source of truth — filters affect only the
	// pretty stdout output.
	fmt.Fprintln(w.logW, line)

	// Capture stderr tail for the diagnose path. We do not separate
	// stdout/stderr here (the pipeline executor pipes both into the
	// same writer); that matches the existing behaviour of pumpLines.
	if w.stderr != nil {
		fmt.Fprintln(w.stderr, line)
	}

	trimmed := strings.TrimSpace(line)
	if isEmptyPkgLine([]byte(line)) {
		// `[no test files]` and EMPTY lines: count once for the summary
		// and suppress from the operator's terminal.
		w.stats.skippedPkgs++
		return
	}
	if trimmed != "" {
		// Ring-buffer the last `tailSize` non-empty lines for the
		// post-step verdict block. Empty lines are skipped so a step
		// that ends with blank padding still surfaces meaningful tail.
		w.pushTail(line)
	}
	// Live mirror to the operator's terminal — indented two spaces so
	// step output reads as content nested under the StepStarted line.
	if w.liveW != nil {
		fmt.Fprintf(w.liveW, "    %s\n", line)
	}
	if goTestOKLine.MatchString(trimmed) {
		w.stats.passed++
		return
	}
	if m := goTestFailLineRE.FindStringSubmatch(trimmed); m != nil {
		// Close any prior block before starting a new one — Go test can
		// emit multiple FAIL lines back-to-back when the same package
		// has several failing tests.
		w.closeFailBlock()
		w.failPending = true
		w.failTitle = m[1]
		w.failCtx = &strings.Builder{}
		return
	}
	if w.failPending {
		// Stop collecting at the next === RUN / --- PASS / FAIL /
		// "ok"/"FAIL" package summary line. Anything else extends the
		// current block (capped at 32 lines to keep the FAILURES
		// section readable).
		if strings.HasPrefix(trimmed, "=== ") ||
			strings.HasPrefix(trimmed, "--- ") ||
			strings.HasPrefix(trimmed, "FAIL\t") ||
			strings.HasPrefix(trimmed, "ok  \t") ||
			strings.HasPrefix(trimmed, "FAIL ") {
			w.closeFailBlock()
			// Fall through so the boundary line is still surfaced if
			// it carries information (a new FAIL block, package
			// summary line, etc.).
		} else {
			if w.failCtx.Len() < 4096 {
				w.failCtx.WriteString(line)
				w.failCtx.WriteByte('\n')
			}
			return
		}
	}
}

// closeFailBlock turns the currently accumulated FAIL context into a
// reporter.Failure and resets the collector. No-op when no block is
// active.
func (w *stepLineWriter) closeFailBlock() {
	if !w.failPending {
		return
	}
	body := strings.TrimRight(w.failCtx.String(), "\n")
	// Pick the first file:line we can find for the Location field so
	// the FAILURES section gets a click-thru target.
	location := ""
	for _, ln := range strings.Split(body, "\n") {
		if m := goTestFileLineRE.FindStringSubmatch(ln); m != nil {
			location = m[1]
			break
		}
	}
	w.stats.failures = append(w.stats.failures, reporter.Failure{
		Title:    w.failTitle,
		Message:  trimFailMessage(body),
		Location: location,
	})
	w.failPending = false
	w.failTitle = ""
	w.failCtx = nil
}

// trimFailMessage keeps the FAILURES body short: the first 6 lines of
// the captured block. The full body is still in the log file for
// power users. Stacktrace frames (multi-line panic dumps) are the
// usual reason a Go test failure block balloons past a screenful; in
// the default (non --debug) view we want the assertion error, not the
// stack.
func trimFailMessage(body string) string {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	const keep = 6
	if len(lines) <= keep {
		return body
	}
	return strings.Join(lines[:keep], "\n") + "\n…"
}

// toolLogWriter intercepts the lines written by the toolResolver
// (pipeline_tools_resolve.go) and translates them into reporter
// SubAction bullets when they carry user-relevant signal, routing the
// noisy variants (paths, `X -> /cache/...`) to the log file only.
//
// The resolver currently writes lines of the form:
//
//	[sklp] tools: <name>@<version> (host: <path>)
//	[sklp] tools: installing <name>@<version> (native)
//	[sklp] tools: <name> -> <path>
//	[sklp] tools: installing <name>@<version> (skt)
//	[sklp] tools: <name> -> <path> (skt)
//	[sklp] auto-install: <command>
//
// Only the "installing" + "auto-install" variants are surfaced as
// SubAction bullets; the others go to the log file only so the
// operator's terminal stays clean.
type toolLogWriter struct {
	mu     sync.Mutex
	rep    reporter.Reporter
	logW   io.Writer
	liveW  io.Writer // pre-step live sink (typically the runner's stderr / Pod log stream)
	buf    bytes.Buffer
	stepMu *sync.Mutex // guards "step active" so we don't print a SubAction outside a step
	active *bool
}

// newToolLogWriter wires the tool-resolver's `[sklp] tools: …` lines
// into three sinks:
//   - logW: the per-run log file. Always written, even when no step is
//     active and even when the line is "noisy" (host hit / path lookup).
//   - rep:  the active step's SubAction bullet. Only used for the
//     "installing …" + "auto-install" variants, and only when a step
//     is on screen (`active == true`).
//   - liveW: a pre-step live sink, used when `active == false` so the
//     operator watching the live tail (or the K8s Pod stream feeding
//     the dash) can see *something* during the seconds the resolver
//     spends downloading toolchains BEFORE the first StepStarted.
//     Pass the runner's stderr here — it is what the spawner tees into
//     captureBuf + LiveOut.
func newToolLogWriter(rep reporter.Reporter, logW, liveW io.Writer, stepMu *sync.Mutex, active *bool) *toolLogWriter {
	return &toolLogWriter{rep: rep, logW: logW, liveW: liveW, stepMu: stepMu, active: active}
}

var (
	reToolsInstalling = regexp.MustCompile(`^\[sklp\] tools: installing (\S+)@(\S+)`)
	reAutoInstall     = regexp.MustCompile(`^\[sklp\] auto-install: (.+)$`)
)

func (w *toolLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		raw := w.buf.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := string(raw[:i])
		w.buf.Next(i + 1)
		w.dispatch(line)
	}
	return len(p), nil
}

func (w *toolLogWriter) dispatch(line string) {
	// Everything goes to the log file. Always.
	fmt.Fprintln(w.logW, line)

	w.stepMu.Lock()
	active := *w.active
	w.stepMu.Unlock()

	mInst := reToolsInstalling.FindStringSubmatch(line)
	mAuto := reAutoInstall.FindStringSubmatch(line)

	if !active {
		// Tool resolution typically happens BEFORE the first step's
		// StepStarted (lazy resolver). Without a step to attach a
		// SubAction to, we still want the operator's live tail to show
		// progress — otherwise the K8s Pod log stream sits silent for
		// 10–30s while we download go/node/pnpm and the dash shows
		// only `sklp pipeline · 2 step(s)`.
		//
		// Full pass-through pre-step: mirror EVERY line to liveW, not
		// just `[sklp] installing` / `auto-install` markers. Auto-install
		// commands (e.g. `go mod download -x`, `pnpm install`) emit
		// their own progress stream on stdout/stderr and the operator
		// needs to see those lines as they happen — a silent 2-minute
		// gap looks identical to a hung pipeline. Volume cost is
		// bounded: this branch only fires during the pre-step toolchain
		// phase, before the first user step starts.
		if w.liveW != nil {
			fmt.Fprintln(w.liveW, line)
		}
		return
	}

	if mInst != nil {
		w.rep.SubAction(fmt.Sprintf("installing %s %s", mInst[1], mInst[2]))
		return
	}
	if mAuto != nil {
		w.rep.SubAction(fmt.Sprintf("auto-install: %s", strings.TrimSpace(mAuto[1])))
		return
	}
	// Other [sklp] tools: lines (host hits, "X -> /path") stay log-only.
}

// Flush drains a buffered trailing partial line.
func (w *toolLogWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		return
	}
	w.dispatch(w.buf.String())
	w.buf.Reset()
}
