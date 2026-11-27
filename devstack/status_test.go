package devstack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestStacksReportsLivenessAndTheNewestRunLog(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	repo := t.TempDir()
	if err := writeState(repo, "web", runState{SupervisorPID: os.Getpid(), Repo: repo, Stack: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := writeState(repo, "api", runState{SupervisorPID: 1 << 22, Repo: repo, Stack: "api"}); err != nil {
		t.Fatal(err)
	}
	logDir := pipeline.CentralLogDir(repo)
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(logDir, devLogName("web", "20261126-090000"))
	newer := filepath.Join(logDir, devLogName("web", "20261126-100000"))
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("web | up\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := Stacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Stack != "api" || got[0].Running || got[0].Log != "" {
		t.Fatalf("api = %+v", got)
	}
	if !got[1].Running || got[1].Log != newer {
		t.Fatalf("web = %+v, want running with log %s", got[1], newer)
	}
}
