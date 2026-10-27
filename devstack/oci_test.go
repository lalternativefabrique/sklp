package devstack

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestBuildOCIArgv_HostMode(t *testing.T) {
	rt := &ociRuntime{Name: "docker"}
	svc := pipeline.Service{
		Image: "postgres:16-alpine",
		Ports: []string{"5432:5432"},
		Volumes: []string{
			"pg-data:/var/lib/postgresql/data",
		},
	}
	env := map[string]string{
		"POSTGRES_USER":     "app",
		"POSTGRES_PASSWORD": "secret",
	}
	// Host mode: --network host, no -p (host ports were removed entirely).
	got := buildOCIArgv(rt, svc, "postgres", env, NetworkHost, "")
	joined := strings.Join(got, " ")
	wants := []string{
		"docker run --rm",
		"--name sklp-dev-postgres",
		"--network host",
		"-e POSTGRES_PASSWORD=secret",
		"-e POSTGRES_USER=app",
		"-v pg-data:/var/lib/postgresql/data",
		"postgres:16-alpine",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("argv missing %q\ngot: %s", w, joined)
		}
	}
	if strings.Contains(joined, "-p 5432:5432") {
		t.Errorf("host mode must not emit -p, got: %s", joined)
	}
}

func TestBuildOCIArgv_BridgeMode(t *testing.T) {
	rt := &ociRuntime{Name: "docker"}
	svc := pipeline.Service{Image: "postgres:16-alpine"}
	got := buildOCIArgv(rt, svc, "postgres", nil, NetworkBridge, "sklp-dev-abc")
	joined := strings.Join(got, " ")
	wants := []string{
		"--network sklp-dev-abc",
		"--hostname postgres",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("argv missing %q\ngot: %s", w, joined)
		}
	}
	if strings.Contains(joined, "--network host") {
		t.Errorf("bridge mode must not emit --network host, got: %s", joined)
	}
	// Host ports were removed entirely — bridge mode must never publish -p.
	if strings.Contains(joined, "-p ") {
		t.Errorf("bridge mode must not emit -p (host ports removed), got: %s", joined)
	}
}

func TestBuildOCIArgv_DeterministicEnvOrder(t *testing.T) {
	// env keys must come out alphabetically so two runs produce the
	// same argv (helps log diffing + tests).
	rt := &ociRuntime{Name: "docker"}
	svc := pipeline.Service{Image: "alpine"}
	env := map[string]string{"Z_LAST": "z", "A_FIRST": "a", "M_MID": "m"}
	got := buildOCIArgv(rt, svc, "x", env, NetworkHost, "")
	idxA := indexOf(got, "A_FIRST=a")
	idxM := indexOf(got, "M_MID=m")
	idxZ := indexOf(got, "Z_LAST=z")
	if idxA < 0 || idxM < 0 || idxZ < 0 || !(idxA < idxM && idxM < idxZ) {
		t.Errorf("env not alphabetical: %v", got)
	}
}

func TestOCIShellCommand_EscapesSingleQuotes(t *testing.T) {
	got := ociShellCommand([]string{"docker", "run", "--name", "weird'name"})
	want := `'docker' 'run' '--name' 'weird'\''name'`
	if got != want {
		t.Errorf("\nwant %q\n got %q", want, got)
	}
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
