package space

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeBackend is a scriptable probeBackend: each probe returns true only
// after readyAfter calls, so tests exercise the polling loop without a real
// container or socket.
type fakeBackend struct {
	readyAfter int
	calls      int
	logBuf     string
}

func (f *fakeBackend) tick() bool {
	f.calls++
	return f.calls >= f.readyAfter
}
func (f *fakeBackend) dialTCP(string) bool  { return f.tick() }
func (f *fakeBackend) getHTTP(string) bool  { return f.tick() }
func (f *fakeBackend) statFile(string) bool { return f.tick() }
func (f *fakeBackend) logTail() string      { return "" }

func (f *fakeBackend) logMatches(re *regexp.Regexp) bool {
	f.calls++
	return re.MatchString(f.logBuf)
}

func TestClassify_PriorityOrder(t *testing.T) {
	r := &ServiceReady{HTTP: "h", TCP: "t", Log: "l", File: "f"}
	if k, target := classify(r); k != kindHTTP || target != "h" {
		t.Errorf("HTTP must win: %v %q", k, target)
	}
	if k, _ := classify(&ServiceReady{TCP: "t", Log: "l"}); k != kindTCP {
		t.Errorf("TCP must win over Log")
	}
	if k, _ := classify(&ServiceReady{Log: "l", File: "f"}); k != kindLog {
		t.Errorf("Log must win over File")
	}
	if k, _ := classify(&ServiceReady{File: "f"}); k != kindFile {
		t.Errorf("File must classify as kindFile")
	}
	if k, _ := classify(nil); k != kindNone {
		t.Errorf("nil must be kindNone")
	}
	if k, _ := classify(&ServiceReady{}); k != kindNone {
		t.Errorf("empty must be kindNone")
	}
}

func TestReadyTimeout_DefaultAndParse(t *testing.T) {
	if d := readyTimeout(nil); d != defaultReadyTimeout {
		t.Errorf("nil timeout = %v, want default", d)
	}
	if d := readyTimeout(&ServiceReady{Timeout: "5s"}); d != 5*time.Second {
		t.Errorf("parsed timeout = %v, want 5s", d)
	}
	if d := readyTimeout(&ServiceReady{Timeout: "garbage"}); d != defaultReadyTimeout {
		t.Errorf("bad timeout = %v, want default fallback", d)
	}
	if d := readyTimeout(&ServiceReady{Timeout: "-1s"}); d != defaultReadyTimeout {
		t.Errorf("negative timeout = %v, want default fallback", d)
	}
}

func TestWaitReady_NilIsImmediate(t *testing.T) {
	if err := waitReady(context.Background(), "svc", nil, &fakeBackend{}); err != nil {
		t.Fatalf("nil probe must return immediately: %v", err)
	}
}

func TestWaitReady_PollsUntilReady(t *testing.T) {
	b := &fakeBackend{readyAfter: 3}
	err := waitReady(context.Background(), "db", &ServiceReady{TCP: "127.0.0.1:5432"}, b)
	if err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if b.calls < 3 {
		t.Errorf("expected >=3 probe attempts, got %d", b.calls)
	}
}

func TestWaitReady_Timeout(t *testing.T) {
	b := &fakeBackend{readyAfter: 1 << 30} // never ready
	err := waitReady(context.Background(), "db", &ServiceReady{TCP: "x:1", Timeout: "150ms"}, b)
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("error should name the service: %v", err)
	}
}

func TestWaitReady_LogMatch(t *testing.T) {
	b := &fakeBackend{logBuf: "database system is ready to accept connections"}
	err := waitReady(context.Background(), "db", &ServiceReady{Log: "ready to accept"}, b)
	if err != nil {
		t.Fatalf("log probe should match: %v", err)
	}
}

func TestEvalProbe_BadLogRegex(t *testing.T) {
	_, err := evalProbe(kindLog, "(unterminated", &fakeBackend{})
	if err == nil || !strings.Contains(err.Error(), "invalid log readiness regex") {
		t.Fatalf("expected bad-regex error, got %v", err)
	}
}

func TestOSProbeBackend_FileAndLog(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "svc.log")
	if err := os.WriteFile(logPath, []byte("listening on 5432\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := osProbeBackend{logPath: logPath}
	if !b.statFile(logPath) {
		t.Error("statFile must find an existing file")
	}
	if b.statFile(filepath.Join(dir, "missing")) {
		t.Error("statFile must report a missing file as not-ready")
	}
	if !b.logMatches(regexp.MustCompile("listening on")) {
		t.Error("logMatches must match captured log content")
	}
	if b.logMatches(regexp.MustCompile("never appears")) {
		t.Error("logMatches must not match absent content")
	}
}

func TestWaitReady_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &fakeBackend{readyAfter: 1 << 30}
	err := waitReady(ctx, "db", &ServiceReady{TCP: "x:1", Timeout: "10s"}, b)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

// recordingProber is a spaceProber that records the targets it was asked to
// dial and answers from a script. It proves the space probe backend dials the
// CONTAINER IP from the bridge netns — never `runc exec` inside the container.
type recordingProber struct {
	tcpReady  bool
	httpReady bool
	tcpAddrs  []string
	httpURLs  []string
}

func (p *recordingProber) probeTCP(addr string) bool {
	p.tcpAddrs = append(p.tcpAddrs, addr)
	return p.tcpReady
}
func (p *recordingProber) probeHTTP(url string) bool {
	p.httpURLs = append(p.httpURLs, url)
	return p.httpReady
}

// TestSpaceProbeBackend_RewritesHostToContainerIP is the FIX-1 unit guard: a
// probe declared against "localhost" / a bare port / a service name must dial
// the container's BRIDGE IP from the space netns, with no nc/wget dependency.
func TestSpaceProbeBackend_RewritesHostToContainerIP(t *testing.T) {
	const ip = "10.88.0.7"
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"localhost host", "localhost:5432", "10.88.0.7:5432"},
		{"empty host", ":5432", "10.88.0.7:5432"},
		{"bare port", "5432", "10.88.0.7:5432"},
		{"loopback ip", "127.0.0.1:6379", "10.88.0.7:6379"},
		{"service name", "db:5432", "10.88.0.7:5432"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProber{tcpReady: true}
			b := spaceProbeBackend{prober: p, ip: ip}
			if !b.dialTCP(tc.target) {
				t.Fatalf("dialTCP(%q) = false, want ready", tc.target)
			}
			if len(p.tcpAddrs) != 1 || p.tcpAddrs[0] != tc.want {
				t.Fatalf("dialed %v, want [%q] (container IP from space netns)", p.tcpAddrs, tc.want)
			}
		})
	}
}

func TestSpaceProbeBackend_HTTPRewritesHost(t *testing.T) {
	p := &recordingProber{httpReady: true}
	b := spaceProbeBackend{prober: p, ip: "10.88.0.9"}
	if !b.getHTTP("http://localhost:8080/health") {
		t.Fatal("getHTTP = false, want ready")
	}
	if len(p.httpURLs) != 1 || p.httpURLs[0] != "http://10.88.0.9:8080/health" {
		t.Fatalf("GET %v, want [http://10.88.0.9:8080/health]", p.httpURLs)
	}
}

// TestSpaceProbeBackend_HTTPBarePathBuildsURL is the regression guard for the
// HTTP probe that never dialled: a bare-path target ("/health") carries no
// host/port, so it must be assembled into http://<ip>:<port>/health from the
// service's declared port. Before the fix the bare path was dialled verbatim
// and could never succeed.
func TestSpaceProbeBackend_HTTPBarePathBuildsURL(t *testing.T) {
	cases := []struct {
		name   string
		target string
		port   int
		want   string
	}{
		{"path with port", "/health", 4100, "http://10.88.0.5:4100/health"},
		{"root path with port", "", 4100, "http://10.88.0.5:4100/"},
		{"path no port", "/health", 0, "http://10.88.0.5/health"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingProber{httpReady: true}
			b := spaceProbeBackend{prober: p, ip: "10.88.0.5", port: tc.port}
			if !b.getHTTP(tc.target) {
				t.Fatalf("getHTTP(%q) = false, want ready", tc.target)
			}
			if len(p.httpURLs) != 1 || p.httpURLs[0] != tc.want {
				t.Fatalf("GET %v, want [%q]", p.httpURLs, tc.want)
			}
		})
	}
}

// TestSpaceProbeBackend_PropagatesNotReady confirms a refused dial surfaces as
// not-ready (the polling loop keeps waiting), not a false positive.
func TestSpaceProbeBackend_PropagatesNotReady(t *testing.T) {
	p := &recordingProber{tcpReady: false}
	b := spaceProbeBackend{prober: p, ip: "10.88.0.2"}
	if b.dialTCP("localhost:5432") {
		t.Fatal("dialTCP must report not-ready when the prober refuses")
	}
}

// TestWaitServiceReady_UsesSpaceProbe wires WaitServiceReady with a space
// prober and asserts it goes through the native dial (not the runc-exec path):
// a runc binary that does not exist would error the exec path, but the space
// path never touches it.
func TestWaitServiceReady_UsesSpaceProbe(t *testing.T) {
	p := &recordingProber{tcpReady: true}
	coords := ProbeCoords{
		RuncBin:     "/nonexistent/runc", // would fail if the exec path ran
		ID:          "sklp-deploy-db-0",
		SpaceProbe:  p,
		ContainerIP: "10.88.0.3",
	}
	err := WaitServiceReady(context.Background(), "db-0", &ServiceReady{TCP: "localhost:5432", Timeout: "2s"}, coords)
	if err != nil {
		t.Fatalf("WaitServiceReady via space probe: %v", err)
	}
	if len(p.tcpAddrs) == 0 || p.tcpAddrs[0] != "10.88.0.3:5432" {
		t.Fatalf("space probe not used; dialed %v", p.tcpAddrs)
	}
}
