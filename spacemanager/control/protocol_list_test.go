package control

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestListSpacesSkipsWorkerSockets(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t))
	dir := socketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"production.sock", "space-attach-ce0bdbc916aa.sock", "space-ingress-ce0bdbc916aa.sock"} {
		ln, err := net.Listen("unix", filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
	}
	got, err := ListSpaces()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"production"}; !slices.Equal(got, want) {
		t.Fatalf("ListSpaces = %v, want %v", got, want)
	}
}

// shortTempDir stays under the 108-byte unix socket path cap t.TempDir can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sklp-ctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
