package pipeline

import (
	"bytes"
	"fmt"
	"io"
)

// Sentinel markers wrap the final JSON report when it is also mirrored
// on the stderr stream. Backends that cannot split stdout from stderr
// (notably Kubernetes pods/log, which interleaves both into a single
// stream) parse the bytes between these markers to recover the report.
//
// The DockerSpawner does not need the markers — it reads stdout
// directly — but the cost of emitting them on stderr is one extra line
// before and after the report, which the SSE / log persistence layer
// surfaces verbatim and the operator quickly learns to ignore.
//
// The chosen markers are deliberately unusual: containing both ===
// fences and the literal "__SKLP_RUN_REPORT__" token, they will not
// collide with anything a build step legitimately prints. Keep them in
// sync with the spawner parser (apps/core/ci/infrastructure).
const (
	ReportSentinelBegin = "===__SKLP_RUN_REPORT_BEGIN__==="
	ReportSentinelEnd   = "===__SKLP_RUN_REPORT_END__==="
)

// emitJSONReport writes the final JSON report to stdout (unchanged
// historical behaviour) AND wraps it on stderr between the sentinel
// markers so single-stream backends can extract it. The trailing
// newline after stdout's body is preserved.
//
// Both writes are best-effort: a closed sink does not abort the run.
func emitJSONReport(stdout, stderr io.Writer, jsonBytes []byte) {
	if stdout != nil {
		_, _ = stdout.Write(jsonBytes)
		if !bytes.HasSuffix(jsonBytes, []byte("\n")) {
			_, _ = stdout.Write([]byte("\n"))
		}
	}
	if stderr != nil {
		fmt.Fprintln(stderr, ReportSentinelBegin)
		_, _ = stderr.Write(jsonBytes)
		if !bytes.HasSuffix(jsonBytes, []byte("\n")) {
			_, _ = stderr.Write([]byte("\n"))
		}
		fmt.Fprintln(stderr, ReportSentinelEnd)
	}
}

// ExtractReportFromMixedStream scans buf (typically the captured
// stdout+stderr of a runner container) for the last well-formed
// sentinel-delimited JSON block and returns its body. It returns
// ("", false) when no block is found or the markers are unbalanced.
//
// Exported because the K8s spawner — which lives in a different
// module path — needs it. The lookup is from the tail of the buffer
// to avoid mismatches if a verbose build step happens to print the
// begin marker but not the end (the runner's emit always pairs them).
func ExtractReportFromMixedStream(buf []byte) (string, bool) {
	endIdx := bytes.LastIndex(buf, []byte(ReportSentinelEnd))
	if endIdx < 0 {
		return "", false
	}
	beginIdx := bytes.LastIndex(buf[:endIdx], []byte(ReportSentinelBegin))
	if beginIdx < 0 {
		return "", false
	}
	body := buf[beginIdx+len(ReportSentinelBegin) : endIdx]
	return string(bytes.TrimSpace(body)), true
}
