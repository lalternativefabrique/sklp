package pipeline

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGoWorkSumKeptRestoresTheFile(t *testing.T) {
	cases := []struct {
		name   string
		before *string
	}{
		{"existing", ptr("original\n")},
		{"absent", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sum := filepath.Join(dir, "go.work.sum")
			if tc.before != nil {
				if err := os.WriteFile(sum, []byte(*tc.before), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", "-c", goWorkSumKept("echo added >> go.work.sum; exit 3"))
			cmd.Dir = dir
			err := cmd.Run()
			if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
				t.Fatalf("exit code not kept: %v", err)
			}
			got, readErr := os.ReadFile(sum)
			if tc.before == nil {
				if !os.IsNotExist(readErr) {
					t.Fatalf("go.work.sum should not exist, got %q", got)
				}
				return
			}
			if string(got) != *tc.before {
				t.Fatalf("go.work.sum = %q, want %q", got, *tc.before)
			}
		})
	}
}

func ptr(s string) *string { return &s }
