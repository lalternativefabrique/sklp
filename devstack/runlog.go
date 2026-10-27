package devstack

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// devLogName builds a dev run-log filename: dev-<stack>-<ts>.log. It mirrors
// pipeline.ciLogName so both kinds of run-log sort together and share one
// naming convention in the central log dir.
func devLogName(stack, ts string) string {
	if s := pipeline.TaskFilenameSlug(stack); s != "" {
		return fmt.Sprintf("dev-%s-%s.log", s, ts)
	}
	return fmt.Sprintf("dev-%s.log", ts)
}

// openDevRunLog creates the run-log for one `sklp dev` launch under the repo's
// central log dir — the same tree the CI reporter writes to and the cockpit
// reads. A dev stack's service output only ever existed on the launching
// terminal's stdout, so a backgrounded stack that died left nothing to inspect.
//
// Failure to open is not fatal: the stack still runs, it just gets no run-log.
func openDevRunLog(repoPath, stack string) (io.WriteCloser, string) {
	dir := pipeline.CentralLogDir(repoPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, ""
	}
	path := filepath.Join(dir, devLogName(stack, time.Now().Format("20060102-150405")))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, ""
	}
	return f, path
}

// teeRunLog wraps stdout/stderr so everything the stack prints also lands in
// the run-log. The returned closer flushes and closes the file; it is nil when
// no log could be opened, in which case the writers are returned unchanged.
func teeRunLog(stdout, stderr io.Writer, repoPath, stack string) (io.Writer, io.Writer, io.Closer, string) {
	f, path := openDevRunLog(repoPath, stack)
	if f == nil {
		return stdout, stderr, nil, ""
	}
	return io.MultiWriter(stdout, f), io.MultiWriter(stderr, f), f, path
}
