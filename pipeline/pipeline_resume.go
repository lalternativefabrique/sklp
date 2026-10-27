package pipeline

import (
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// resumeStatus enumerates the three states the resume routing can be
// in for the current plan key. Surfaced to the user as a single-line
// info log before ExecuteCI runs.
type resumeStatus int

const (
	resumeStatusFirstRun   resumeStatus = iota // no last.json under .sklp/cache/plans/
	resumeStatusKeyChanged                     // some plan(s) have records, but not the current key
	resumeStatusFound                          // last.json exists for the current plan key
)

// planResumeStatus inspects the on-disk last-run records and reports
// how `--resume` will behave for the given plan key. We use it only
// for the human-readable info log; the executor still does its own
// LoadLastRun and decides per-step.
func planResumeStatus(repoAbs, planKey string) resumeStatus {
	if _, err := plan.LoadLastRun(repoAbs, planKey); err == nil {
		return resumeStatusFound
	}
	// No record for our key. Distinguish "first run on this repo"
	// (plans/ dir empty or absent) from "plan key changed" (plans/
	// has at least one sibling entry).
	plansDir := filepath.Join(repoAbs, ".sklp", "cache", "plans")
	entries, err := os.ReadDir(plansDir)
	if err != nil || len(entries) == 0 {
		return resumeStatusFirstRun
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != planKey {
			return resumeStatusKeyChanged
		}
	}
	return resumeStatusFirstRun
}
