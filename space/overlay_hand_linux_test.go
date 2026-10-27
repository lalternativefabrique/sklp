package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandOverlayDirsToChild_OnceAtBirthNeverAgain(t *testing.T) {
	base := t.TempDir()
	upper, work, merged := filepath.Join(base, "upper"), filepath.Join(base, "work"), filepath.Join(base, "merged")
	for _, d := range []string{upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	hand := func(paths ...string) error { calls++; return nil }

	if err := handOverlayDirsToChild(231068, upper, work, merged, hand); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a newborn upper is handed once, got %d calls", calls)
	}

	if err := handOverlayDirsToChild(os.Getuid(), upper, work, merged, hand); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("an upper the root already owns must not be handed again, got %d calls", calls)
	}

	stale := filepath.Join(upper, "run", "mysqld", "mysqld.pid")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("1"), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := handOverlayDirsToChild(231068, upper, work, merged, hand); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("an upper populated by another owner is discarded and handed, got %d calls", calls)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the foreign upper's content must be gone: %v", err)
	}
}
