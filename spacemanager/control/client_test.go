package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cannedServer mounts a minimal mux on a UDS at the space's socket path,
// returning the listener and a stop func. It mirrors the real routes so the
// client is tested end-to-end without depending on server.go (which is gated).
func cannedServer(t *testing.T, space string, handler http.Handler) func() {
	t.Helper()
	// Point SocketPath at a temp dir so Dial finds this canned server.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	socket := SocketPath(space)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	return func() { _ = srv.Close() }
}

func TestClientHealthAndState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteHealth, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HealthResponse{Space: "production", PID: 42, Since: "2026-05-31T00:00:00Z"})
	})
	mux.HandleFunc(RouteState, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StateResponse{
			Space:    "production",
			AllReady: true,
			Services: []ServiceState{{
				Name:      "api",
				State:     "ready",
				Instances: []InstanceState{{ID: "api-0", State: "ready", PID: 1234}},
			}},
		})
	})
	stop := cannedServer(t, "production", mux)
	defer stop()

	c, err := Dial("production")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h.PID != 42 || h.Space != "production" {
		t.Fatalf("health = %+v", h)
	}

	s, err := c.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !s.AllReady || len(s.Services) != 1 || s.Services[0].Instances[0].PID != 1234 {
		t.Fatalf("state = %+v", s)
	}
}

func TestClientLogsDump(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteLogs, func(w http.ResponseWriter, r *http.Request) {
		svc := strings.TrimPrefix(r.URL.Path, RouteLogs)
		if svc != "api" {
			http.Error(w, "unknown", http.StatusNotFound)
			return
		}
		w.Write([]byte("[api] line one\n[api] line two\n"))
	})
	stop := cannedServer(t, "production", mux)
	defer stop()

	c, _ := Dial("production")
	var buf bytes.Buffer
	if err := c.Logs(context.Background(), "api", false, &buf); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Fatalf("logs dump = %q", got)
	}
}

func TestClientLogsUnknownService404(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteLogs, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown service", http.StatusNotFound)
	})
	stop := cannedServer(t, "production", mux)
	defer stop()

	c, _ := Dial("production")
	err := c.Logs(context.Background(), "ghost", false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want 404 error mentioning service, got %v", err)
	}
}

// TestClientLogsFollowCtxCancel: a follow stream stops when the client cancels
// its context — no goroutine blocks forever. The canned server streams until
// its handler context (the request) is done.
func TestClientLogsFollowCtxCancel(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	mux := http.NewServeMux()
	mux.HandleFunc(RouteLogs, func(w http.ResponseWriter, r *http.Request) {
		defer wg.Done()
		fl, _ := w.(http.Flusher)
		w.Write([]byte("[api] streaming\n"))
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done() // hold the stream open until the client disconnects
	})
	stop := cannedServer(t, "production", mux)
	defer stop()

	c, _ := Dial("production")
	ctx, cancel := context.WithCancel(context.Background())
	var buf safeBuf
	done := make(chan error, 1)
	go func() { done <- c.Logs(ctx, "api", true, &buf) }()

	// Wait for the first line, then cancel.
	deadline := time.After(2 * time.Second)
	for buf.Len() == 0 {
		select {
		case <-deadline:
			t.Fatal("never received first line")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("logs follow returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Logs did not return after ctx cancel (follower leak)")
	}
	// Server handler must also unwind (its request context cancelled).
	doneServer := make(chan struct{})
	go func() { wg.Wait(); close(doneServer) }()
	select {
	case <-doneServer:
	case <-time.After(2 * time.Second):
		t.Fatal("server handler did not unwind after client cancel")
	}
}

// TestClientDownAck: Down posts to /v1/down and is satisfied by a 202 +
// {stopping:true} ack. A non-202 or a stopping=false ack is an error.
func TestClientDownAck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteDown, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(DownResponse{Stopping: true})
	})
	stop := cannedServer(t, "production", mux)
	defer stop()

	c, _ := Dial("production")
	if err := c.Down(context.Background()); err != nil {
		t.Fatalf("down: %v", err)
	}
}

// TestClientDownNoSupervisor: Down against a missing socket is ErrNoSupervisor,
// so the CLI can fall back to the pidfile signal.
func TestClientDownNoSupervisor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no socket
	c, _ := Dial("ghost")
	err := c.Down(context.Background())
	if !errors.Is(err, ErrNoSupervisor) {
		t.Fatalf("want ErrNoSupervisor, got %v", err)
	}
}

// TestClientWaitGone: WaitGone returns once the socket stops answering. We close
// the canned server mid-wait and expect a prompt, nil return.
func TestClientWaitGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteHealth, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HealthResponse{Space: "production"})
	})
	stop := cannedServer(t, "production", mux)

	c, _ := Dial("production")
	go func() { time.Sleep(150 * time.Millisecond); stop() }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.WaitGone(ctx); err != nil {
		t.Fatalf("WaitGone did not observe the socket disappear: %v", err)
	}
}

// TestClientNoSupervisor: dialing a space with no socket yields a
// distinguishable ErrNoSupervisor.
func TestClientNoSupervisor(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // empty: no socket created
	c, err := Dial("ghost")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_, err = c.State(context.Background())
	if !errors.Is(err, ErrNoSupervisor) {
		t.Fatalf("want ErrNoSupervisor, got %v", err)
	}
}

// safeBuf is a tiny concurrency-safe buffer for the follow test.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// A 404 on a service lookup must not blame a scope the caller never chose. The
// network transport carries no space name (the listener is scoped server-side),
// so naming an empty space there reads as a bug in the request.
func TestUnknownServiceErrNamesTheNodeWhenSpaceless(t *testing.T) {
	spaceless := (&Client{}).unknownServiceErr("core-0")
	if strings.Contains(spaceless.Error(), `space ""`) {
		t.Errorf("got %q, want no empty space in the message", spaceless)
	}
	if !strings.Contains(spaceless.Error(), "core-0") {
		t.Errorf("got %q, want the service named", spaceless)
	}
	scoped := (&Client{space: "production"}).unknownServiceErr("core-0")
	if !strings.Contains(scoped.Error(), `space "production"`) {
		t.Errorf("got %q, want the space named when it is known", scoped)
	}
}
