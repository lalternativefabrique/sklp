// Package remoteapply builds the request a remote node resolves itself: the raw
// deploy.yaml of a stack and every ${VAR} it references, gathered on the
// operator's machine. `sklp deploy apply` and `sklp mcp` share it.
package remoteapply

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/deployvars"
)

var envMu sync.Mutex

// Resolve returns the path of the stack's deploy.yaml under repo.
func Resolve(repo, stack string) (string, error) {
	return layout.New(repo).Resolve(stack, "deploy")
}

// Source reads the deploy file at path and collects the values of the
// variables it references from project secrets, env_files, stack configs and
// the host env, in that precedence.
func Source(path, stack string, warn io.Writer) (control.ApplySource, error) {
	// The sources are loaded through the process env; a long-lived caller
	// (sklp mcp) must not carry one repo's values into the next apply.
	envMu.Lock()
	defer envMu.Unlock()
	defer restoreEnv(os.Environ())

	LoadProjectSecrets(path, warn)
	raw, err := os.ReadFile(path)
	if err != nil {
		return control.ApplySource{}, fmt.Errorf("read deploy file: %w", err)
	}
	if err := deployvars.LoadSources(path, raw); err != nil {
		return control.ApplySource{}, err
	}
	vars, err := deployvars.Collect(raw, os.LookupEnv)
	if err != nil {
		return control.ApplySource{}, err
	}
	return control.ApplySource{YAML: string(raw), Vars: vars, Stack: stack}, nil
}

func restoreEnv(snapshot []string) {
	os.Clearenv()
	for _, kv := range snapshot {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			_ = os.Setenv(k, v)
		}
	}
}
