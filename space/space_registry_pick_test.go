package space

import (
	"errors"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/spaceapi"
)

func devContainers() []DeployContainerInfo {
	return []DeployContainerInfo{
		{ID: "sklp-deploy-dev-core", Status: "running"},
		{ID: "sklp-deploy-dev-migrate", Status: "stopped"},
		{ID: "sklp-deploy-dev-postgres", Status: "running"},
	}
}

// The service name is what an operator reads off `space get`, so it must resolve
// to the full runc id.
func TestPickContainerByServiceName(t *testing.T) {
	got, err := pickContainer(devContainers(), "dev", "core")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sklp-deploy-dev-core" {
		t.Errorf("got %q, want sklp-deploy-dev-core", got)
	}
}

// The full id is what `space get` PRINTS, so pasting it back must work too.
func TestPickContainerByFullID(t *testing.T) {
	got, err := pickContainer(devContainers(), "dev", "sklp-deploy-dev-postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sklp-deploy-dev-postgres" {
		t.Errorf("got %q, want sklp-deploy-dev-postgres", got)
	}
}

// A converged oneshot must be reported as stopped, NOT silently redirected to a
// running sibling: the caller believes they entered what they asked for.
func TestPickContainerStoppedIsReportedNotRedirected(t *testing.T) {
	got, err := pickContainer(devContainers(), "dev", "migrate")
	if !errors.Is(err, spaceapi.ErrContainerNotRunning) {
		t.Fatalf("err = %v, want ErrContainerNotRunning", err)
	}
	if got != "" {
		t.Errorf("got %q, want no target for a stopped container", got)
	}
}

// A typo must not fall back to whatever runs first.
func TestPickContainerUnknownTarget(t *testing.T) {
	_, err := pickContainer(devContainers(), "dev", "coree")
	if !errors.Is(err, spaceapi.ErrContainerNotFound) {
		t.Fatalf("err = %v, want ErrContainerNotFound", err)
	}
}

// The error names the target the caller typed, so the message is actionable.
func TestPickContainerErrorNamesTheTarget(t *testing.T) {
	_, err := pickContainer(devContainers(), "dev", "coree")
	if err == nil || !strings.Contains(err.Error(), "coree") {
		t.Errorf("err = %v, want it to name coree", err)
	}
}

// A bare `sh` prompt is identical in every container of a space (same image, same
// /workspace), so the interactive shell must carry the service name.
func TestInteractiveShellNamesTheContainer(t *testing.T) {
	got := interactiveShell("postgres")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "postgres") {
		t.Errorf("shell cmd = %v, want it to name postgres", got)
	}
	if !strings.Contains(joined, "exec sh") {
		t.Errorf("shell cmd = %v, want it to exec a shell", got)
	}
}

// Arrow keys print as ^[[A under dash (no line editing), which leaves the line
// unusable. The shell must prefer bash when the image has it, and still fall back
// so an image without bash keeps a working shell.
func TestInteractiveShellPrefersBashWithFallback(t *testing.T) {
	joined := strings.Join(interactiveShell("core"), " ")
	if !strings.Contains(joined, "bash --norc -i") {
		t.Errorf("cmd = %s, want it to prefer an interactive bash", joined)
	}
	if !strings.Contains(joined, "exec sh") {
		t.Errorf("cmd = %s, want an sh fallback for images without bash", joined)
	}
	// --norc matters: /etc/bash.bashrc would otherwise reset PS1 and drop the name.
	if !strings.Contains(joined, "--norc") {
		t.Errorf("cmd = %s, want --norc so the prompt survives bashrc", joined)
	}
}

// An unnamed target still opens a shell rather than a broken command line.
func TestInteractiveShellWithoutLabelStillOpensAShell(t *testing.T) {
	joined := strings.Join(interactiveShell(""), " ")
	if !strings.Contains(joined, "exec sh") {
		t.Errorf("cmd = %s, want a usable shell", joined)
	}
}

func TestServiceFromContainerID(t *testing.T) {
	if got := serviceFromContainerID("sklp-deploy-dev-postgres", "dev"); got != "postgres" {
		t.Errorf("got %q, want postgres", got)
	}
	if got := serviceFromContainerID("foreign-id", "dev"); got != "" {
		t.Errorf("got %q, want empty for an unprefixed id", got)
	}
}
