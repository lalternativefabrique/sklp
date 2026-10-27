//go:build linux

package plan

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func skipUnlessNativeBuildable(t *testing.T) {
	t.Helper()
	out, err := exec.Command("unshare", "--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "true").CombinedOutput()
	if err != nil {
		t.Skipf("rootless userns+subuid unavailable: %v: %s", err, out)
	}
	// Local registry must be up (started by the test harness / a sidecar).
	c := &http.Client{Timeout: 1 * time.Second}
	if r, err := c.Get("http://localhost:5000/v2/"); err != nil || r.StatusCode != 200 {
		t.Skip("no local registry on localhost:5000")
	}
}

// TestRunNativeBuild_BuildExportPush drives the native engine end to end: a
// multi-stage Dockerfile is built, exported to OCI, and pushed to a local
// registry — via the real runPublish native builder. Scan is
// disabled (Trivy not assumed installed); the build + push path is what matters.
func TestRunNativeBuild_BuildExportPush(t *testing.T) {
	skipUnlessNativeBuildable(t)
	if testing.Short() {
		t.Skip("pulls alpine + runs apk; skipped in -short")
	}

	repo := t.TempDir()
	// A 2-stage Dockerfile + a context file the COPY pulls in.
	must(t, os.WriteFile(filepath.Join(repo, "Dockerfile"), []byte(
		`FROM alpine:3.20 AS build
RUN mkdir -p /out && echo native-engine > /out/marker
FROM alpine:3.20
COPY --from=build /out/marker /marker
COPY note.txt /note.txt
ENTRYPOINT ["/bin/cat", "/marker"]
`), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "note.txt"), []byte("from context"), 0o644))

	step := CIStep{
		StepKind: KindCIStepPublish,
		Publish: &PublishSpec{
			Image:       "localhost:5000/native-demo",
			Tag:         "v1",
			Context:     ".",
			Dockerfile:  "Dockerfile",
			Push:        true,
			ScanEnabled: false,
		},
	}
	cfg := CIExecConfig{Repo: repo, BaseEnv: os.Environ()}

	// execFn is only used for the scan phase (disabled here); a stub that fails
	// loudly catches an accidental shell-out.
	stub := func(ctx context.Context, name string, spec sandboxSpec) (int, error) {
		t.Fatalf("native build must not shell out (phase %q)", name)
		return 1, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	code, err := runPublish(ctx, cfg, step, t.TempDir(), os.Stdout, os.Stderr, stub)
	if err != nil || code != 0 {
		t.Fatalf("runPublish(native) = %d, %v", code, err)
	}

	// The image must now exist in the registry.
	c := &http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://localhost:5000/v2/native-demo/tags/list")
	if err != nil {
		t.Fatalf("registry query: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("image not in registry: HTTP %d", r.StatusCode)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
