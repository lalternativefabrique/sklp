package space

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// defaultReadyTimeout caps a probe when ServiceReady.Timeout is empty.
const defaultReadyTimeout = 30 * time.Second

// probeBackend abstracts the side-effecting calls a readiness probe makes
// so the evaluation logic is testable without a real container or socket.
// The production backend (osProbeBackend) hits the network / filesystem;
// tests inject a fake.
type probeBackend interface {
	// dialTCP reports whether a TCP connection to addr succeeds.
	dialTCP(addr string) bool
	// getHTTP reports whether a GET on url returns a 2xx status.
	getHTTP(url string) bool
	// statFile reports whether path exists.
	statFile(path string) bool
	// logMatches reports whether the service log so far matches re.
	logMatches(re *regexp.Regexp) bool
	// logTail returns the end of the service's output, so a readiness
	// timeout can say what the service did instead of only that it did not
	// answer.
	logTail() string
}

// readyKind classifies a ServiceReady into the single probe it drives.
// Exactly one of HTTP / TCP / Log / File is honoured, picked in that
// order, matching the schema doc. kindNone means launch-and-forget.
type readyKind int

const (
	kindNone readyKind = iota
	kindHTTP
	kindTCP
	kindLog
	kindFile
)

// classify picks the active probe from a ServiceReady. A nil probe or one
// with no field set is launch-and-forget.
func classify(r *ServiceReady) (readyKind, string) {
	if r == nil {
		return kindNone, ""
	}
	switch {
	case r.HTTP != "":
		return kindHTTP, r.HTTP
	case r.TCP != "":
		return kindTCP, r.TCP
	case r.Log != "":
		return kindLog, r.Log
	case r.File != "":
		return kindFile, r.File
	default:
		return kindNone, ""
	}
}

// readyTimeout parses ServiceReady.Timeout, falling back to the default on
// an empty or malformed value (a bad duration must not silently disable
// the cap, so we keep the default rather than zero).
func readyTimeout(r *ServiceReady) time.Duration {
	if r == nil || r.Timeout == "" {
		return defaultReadyTimeout
	}
	d, err := time.ParseDuration(r.Timeout)
	if err != nil || d <= 0 {
		return defaultReadyTimeout
	}
	return d
}

// evalProbe runs one probe attempt against the backend. It compiles the
// log regex lazily (only the log kind needs it) and surfaces a bad regex
// as an error rather than a silent never-ready loop.
func evalProbe(kind readyKind, target string, b probeBackend) (bool, error) {
	switch kind {
	case kindNone:
		return true, nil
	case kindHTTP:
		return b.getHTTP(target), nil
	case kindTCP:
		return b.dialTCP(target), nil
	case kindFile:
		return b.statFile(target), nil
	case kindLog:
		re, err := regexp.Compile(target)
		if err != nil {
			return false, fmt.Errorf("invalid log readiness regex %q: %w", target, err)
		}
		return b.logMatches(re), nil
	default:
		return false, fmt.Errorf("unknown readiness kind %d", kind)
	}
}

func checkReady(name string, r *ServiceReady, b probeBackend) error {
	kind, target := classify(r)
	if kind == kindNone {
		return nil
	}
	ok, err := evalProbe(kind, target, b)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service %q probe failed (probe: %s %q)", name, kindName(kind), target)
	}
	return nil
}

// waitReady polls the probe until it passes or the timeout elapses. A nil
// or empty probe returns immediately (launch-and-forget). The ctx cancels
// the wait; the caller is responsible for tearing the service down.
func waitReady(ctx context.Context, name string, r *ServiceReady, b probeBackend) error {
	kind, target := classify(r)
	if kind == kindNone {
		return nil
	}
	timeout := readyTimeout(r)
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		ok, err := evalProbe(kind, target, b)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			err := fmt.Errorf("service %q did not become ready within %s (probe: %s %q)", name, timeout, kindName(kind), target)
			if tail := b.logTail(); tail != "" {
				return fmt.Errorf("%w\n--- %s log ---\n%s", err, name, tail)
			}
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service %q readiness wait cancelled: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// kindName renders a readyKind for error messages.
func kindName(k readyKind) string {
	switch k {
	case kindHTTP:
		return "http"
	case kindTCP:
		return "tcp"
	case kindLog:
		return "log"
	case kindFile:
		return "file"
	default:
		return "none"
	}
}

// execProbeBackend is the production probe backend for an `image:` service
// on the shared netns. TCP/HTTP probes run INSIDE the service container via
// `runc exec` (busybox `nc` / `wget`), so they reach the loopback address
// in the shared netns rather than crossing the user-namespace barrier from
// the host (a privileged operation we deliberately avoid — same trick as
// the multi-container POC's pg_isready). File / Log probes read host-visible
// paths: the container log we capture and the bundle/host filesystem.
type execProbeBackend struct {
	ctx      context.Context
	runcBin  string // extracted runc path
	runcRoot string
	id       string // container id to exec into for net probes
	logPath  string // host-side captured stdout/stderr of the container
}

// dialTCP execs `nc -z host port` inside the container. busybox nc ships in
// virtually every base image; exit 0 means the port accepts a connection.
func (e execProbeBackend) dialTCP(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return runcExec(e.ctx, e.runcBin, e.runcRoot, e.id, "nc", "-z", host, port) == nil
}

// getHTTP execs `wget -q -O- <url>` inside the container; a non-zero exit
// (connection refused, non-2xx with --spider semantics) means not-ready.
// busybox wget fails the process on HTTP errors, which is the signal we want.
func (e execProbeBackend) getHTTP(url string) bool {
	return runcExec(e.ctx, e.runcBin, e.runcRoot, e.id, "wget", "-q", "-O", "/dev/null", url) == nil
}

func (e execProbeBackend) statFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (e execProbeBackend) logMatches(re *regexp.Regexp) bool {
	data, err := os.ReadFile(e.logPath)
	if err != nil {
		return false
	}
	return re.Match(data)
}

// spaceProbeBackend is the production probe backend for a SPACE service. It
// dials the container's bridge IP natively FROM the space netns (via prober),
// so a TCP/HTTP readiness probe needs NO `nc`/`wget` binary in the image — the
// failure mode that left a probe permanently red. The probe target's host
// ("localhost", ":", a bare host, or empty) refers to the container itself and
// is rewritten to the container's bridge IP; the declared port/path is kept.
// File / Log probes read the host-visible captured paths, same as the others.
type spaceProbeBackend struct {
	prober  spaceProber
	ip      string // container bridge IP the probe targets
	port    int    // service's declared listen port, for bare-path HTTP probes
	logPath string
}

func (c spaceProbeBackend) dialTCP(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		// A bare port ("5432") has no host; treat the whole value as the port.
		port = addr
	}
	if port == "" {
		return false
	}
	return c.prober.probeTCP(net.JoinHostPort(c.ip, port))
}

func (c spaceProbeBackend) getHTTP(url string) bool {
	return c.prober.probeHTTP(buildProbeURL(url, c.ip, c.port))
}

func (c spaceProbeBackend) statFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (c spaceProbeBackend) logMatches(re *regexp.Regexp) bool {
	data, err := os.ReadFile(c.logPath)
	if err != nil {
		return false
	}
	return re.Match(data)
}

// buildProbeURL turns a probe's HTTP target into a dialable URL against the
// container's bridge IP. Two shapes are accepted:
//
//   - A BARE PATH ("/health", or "" → "/"): the common deploy case. The schema
//     carries only the path, so we assemble http://<ip>:<port><path> from the
//     service's declared port. This is the case rewriteHostToIP could not handle
//     (url.Parse("/health").Host == "") and is why an HTTP probe never dialled.
//   - A FULL URL ("http://localhost:8080/health"): the declared host refers to
//     the container itself, so we swap it for ip (keeping scheme/port/path/query).
//
// A target that does not parse is returned unchanged so a malformed probe fails
// honestly downstream rather than silently passing.
func buildProbeURL(target, ip string, port int) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	if u.Host == "" {
		// Bare path: build the full URL from ip + service port.
		host := ip
		if port > 0 {
			host = net.JoinHostPort(ip, fmt.Sprintf("%d", port))
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		built := url.URL{Scheme: "http", Host: host, Path: path, RawQuery: u.RawQuery}
		return built.String()
	}
	// Full URL: rewrite its host to the container IP, keeping its own port.
	host := ip
	if p := u.Port(); p != "" {
		host = net.JoinHostPort(ip, p)
	}
	u.Host = host
	return u.String()
}

// osProbeBackend covers probes that the host can satisfy directly (no shared
// netns hop): File existence and Log scanning. Kept for callers/tests that
// only need those without a running container. dialTCP/getHTTP fall back to
// a direct host dial, which only works when the target is host-reachable.
type osProbeBackend struct {
	logPath string
}

func (o osProbeBackend) dialTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (o osProbeBackend) getHTTP(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (o osProbeBackend) statFile(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (o osProbeBackend) logMatches(re *regexp.Regexp) bool {
	data, err := os.ReadFile(o.logPath)
	if err != nil {
		return false
	}
	return re.Match(data)
}

const readinessLogTailBytes = 4000

// tailOfFile returns the last readinessLogTailBytes of path, trimmed, or ""
// when the file cannot be read.
func tailOfFile(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > readinessLogTailBytes {
		data = data[len(data)-readinessLogTailBytes:]
	}
	return strings.TrimSpace(string(data))
}

func (e execProbeBackend) logTail() string  { return tailOfFile(e.logPath) }
func (c spaceProbeBackend) logTail() string { return tailOfFile(c.logPath) }
func (o osProbeBackend) logTail() string    { return tailOfFile(o.logPath) }
