//go:build linux

package imagebuild

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// runWithSecrets executes a RUN that requested --mount=type=secret. We stage the
// secret files at their target paths inside the rootfs, run the command via the
// same unshare+chroot step runner, then remove the secrets — so they never
// persist in the produced rootfs (Docker's secret-mount guarantee). A requested
// secret with no provided value is left absent: the RUN sees an empty file path
// and fails on its own, surfacing a clear "secret X not provided" upstream.
func runWithSecrets(ctx context.Context, rootfs string, r parse.RunSpec, opts Options, steps runnerbuild.StepRunner) error {
	var staged []string
	defer func() {
		for _, p := range staged {
			_ = os.Remove(p)
		}
	}()

	for _, sec := range r.Secrets {
		hostPath, ok := opts.Secrets[sec.ID]
		if !ok {
			return fmt.Errorf("RUN needs secret %q but it was not provided to the build", sec.ID)
		}
		dst := filepath.Join(rootfs, sec.Target)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(hostPath, dst, 0o600); err != nil {
			return fmt.Errorf("stage secret %q: %w", sec.ID, err)
		}
		staged = append(staged, dst)
	}

	// Use the STAGE's configured step runner (base image env/PATH + caches), not
	// a bare one — else a secret RUN on e.g. golang would not find `go` (the
	// image PATH is absent). The secret files exist at their targets for the
	// duration of this single Exec.
	return steps.Exec(ctx, rootfs, []string{r.Cmd})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
