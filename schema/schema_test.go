package schema

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSchemasAreUpToDate(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	before := map[string][]byte{}
	for _, name := range Contracts {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	cmd := exec.Command("go", "run", "./cmd/genschema")
	cmd.Dir = filepath.Dir(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./cmd/genschema: %v\n%s", err, out)
	}
	for name, old := range before {
		now, _ := os.ReadFile(filepath.Join(dir, name))
		if !bytes.Equal(old, now) {
			t.Errorf("%s drifted from its struct: run go generate ./... and commit it", name)
		}
	}
}

func TestEveryContractHasItsSchema(t *testing.T) {
	for contract := range Contracts {
		if raw, ok := Get(contract); !ok || len(raw) == 0 {
			t.Errorf("no schema for %s", contract)
		}
	}
}
