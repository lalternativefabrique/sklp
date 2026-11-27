package repofacts

import (
	"strings"
	"testing"
)

func TestDevConfig_GoApp(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"apps/api/go.mod":  "module example.com/api\n\ngo 1.25\n",
		"apps/api/main.go": "package main\n\nfunc main() {}\n",
	})
	yaml, _ := DevConfig(dir)
	for _, want := range []string{
		"kind: devstack",
		"services:",
		"run: go run .",
		"workdir: apps/api",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("dev.yaml missing %q:\n%s", want, yaml)
		}
	}
}

func TestDevConfig_Deterministic(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"apps/api/go.mod":  "module example.com/api\n\ngo 1.25\n",
		"apps/api/main.go": "package main\n\nfunc main() {}\n",
	})
	a, _ := DevConfig(dir)
	b, _ := DevConfig(dir)
	if a != b {
		t.Errorf("DevConfig not deterministic:\n%s\n---\n%s", a, b)
	}
}

func TestDevConfig_EmptyRepo(t *testing.T) {
	dir := mkRepo(t, nil)
	yaml, notes := DevConfig(dir)
	if !strings.Contains(yaml, "kind: devstack") {
		t.Errorf("should still emit a skeleton:\n%s", yaml)
	}
	if len(notes) == 0 {
		t.Error("expected a NOTE when no app modules were found")
	}
}

func TestDevConfigBindsAKnownDevServerAndPublishesItsPort(t *testing.T) {
	for _, c := range []struct{ dep, run, port string }{
		{"next", "run: pnpm dev -H 0.0.0.0 -p 3000", "3000"},
		{"vite", "run: pnpm dev --host 0.0.0.0 --port 5173", "5173"},
	} {
		dir := mkRepo(t, map[string]string{
			"package.json":   `{"scripts":{"dev":"x"},"dependencies":{"` + c.dep + `":"1"}}`,
			"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
		})
		out, _ := DevConfig(dir)
		for _, w := range []string{c.run, `ports: ["` + c.port + ":" + c.port + `"]`, "tcp: localhost:" + c.port} {
			if !strings.Contains(out, w) {
				t.Errorf("%s draft missing %q:\n%s", c.dep, w, out)
			}
		}
	}
}
