package pipeline

import "testing"

func TestComposeReadyHTTP(t *testing.T) {
	cases := []struct {
		name string
		http string
		port int
		want string
	}{
		{"empty stays empty", "", 4100, ""},
		{"path composed onto port", "/health", 4100, "http://localhost:4100/health"},
		{"path root", "/", 4100, "http://localhost:4100/"},
		{"path but no port -> empty", "/health", 0, ""},
		{"absolute url verbatim", "http://localhost:4100/health", 4100, "http://localhost:4100/health"},
		{"absolute url ignores port arg", "http://localhost:9999/x", 4100, "http://localhost:9999/x"},
		// A named-service host (resolved on the bridge) is not a path → must
		// pass through untouched.
		{"named host passes through", "http://postgres:5432/health", 4100, "http://postgres:5432/health"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ComposeReadyHTTP(c.http, c.port); got != c.want {
				t.Errorf("ComposeReadyHTTP(%q, %d) = %q, want %q", c.http, c.port, got, c.want)
			}
		})
	}
}

func TestPortFromReadyHTTP(t *testing.T) {
	cases := []struct {
		name   string
		port   int
		http   string
		want   int
		wantOk bool
	}{
		{"explicit port wins", 4100, "http://localhost:9999/h", 4100, true},
		{"explicit port wins over path", 5432, "/health", 5432, true},
		{"fallback parses absolute url", 0, "http://localhost:4100/health", 4100, true},
		{"no port, path -> none", 0, "/health", 0, false},
		{"no port, no http -> none", 0, "", 0, false},
		{"absolute url without port -> none", 0, "http://localhost/health", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := PortFromReadyHTTP(c.port, c.http)
			if got != c.want || ok != c.wantOk {
				t.Errorf("PortFromReadyHTTP(%d, %q) = (%d, %v), want (%d, %v)",
					c.port, c.http, got, ok, c.want, c.wantOk)
			}
		})
	}
}
