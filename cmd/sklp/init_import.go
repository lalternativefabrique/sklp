package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/importer"
	"github.com/lalternativefabrique/sklp/internal/repofacts"
	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

type composeImported struct {
	res *importer.ComposeImport
	rel string
}

// importCompose translates the repo's compose files into the stack's
// dev.yaml. An existing dev.yaml is never overwritten without -force: the
// import then lands next to it as dev.yaml.draft.
func importCompose(repoAbs, stack string, force bool) (*composeImported, error) {
	runnables, _ := repofacts.ScanRunnable(repoAbs)
	res, err := importer.ImportCompose(importer.ComposeOptions{Root: repoAbs, Stack: stack, Runnables: runnables})
	if err != nil || res == nil {
		return nil, err
	}
	rel := filepath.Join(".sklp", stack, "dev.yaml")
	if !force && fileExists(filepath.Join(repoAbs, rel)) {
		rel += ".draft"
	}
	if err := writeDraft(filepath.Join(repoAbs, rel), res.YAML, force || strings.HasSuffix(rel, ".draft")); err != nil {
		return nil, err
	}
	return &composeImported{res: res, rel: rel}, nil
}

func runComposeImport(repoAbs, stack string, force, ascii bool) int {
	rep := reporter.New(os.Stderr, reporter.Options{ASCII: ascii})
	rep.Header("sklp init -from compose")
	start := time.Now()
	rep.StepStarted("importing compose")
	imp, err := importCompose(repoAbs, stack, force)
	if err != nil {
		rep.StepFinished("importing compose", reporter.StatusFailed, time.Since(start), "")
		fmt.Fprintf(os.Stderr, "sklp init: %v\n", err)
		return 1
	}
	if imp == nil {
		rep.StepFinished("importing compose", reporter.StatusFailed, time.Since(start), "")
		fmt.Fprintf(os.Stderr, "sklp init: no compose.yaml / docker-compose.yml at %s\n", repoAbs)
		return 1
	}
	manual := importer.ManualCount(imp.res.Notes)
	rep.StepFinished("importing compose", reporter.StatusPassed, time.Since(start), fmt.Sprintf("%d manual action(s)", manual))
	next := fmt.Sprintf("sklp dev %s", stack)
	if manual > 0 {
		next = fmt.Sprintf("resolve TODO(import) in %s · sklp dev %s --validate", imp.rel, stack)
	}
	rep.Finalize("IMPORT", reporter.StatusPassed, time.Since(start), nil,
		[]reporter.FinalField{{Label: "next", Value: next}})
	printImportReport(os.Stderr, imp)
	return 0
}

func printImportReport(w io.Writer, imp *composeImported) {
	if imp == nil {
		return
	}
	notes := imp.res.Notes
	manual := importer.ManualCount(notes)
	fmt.Fprintf(w, "\ncompose import: %s <- %s\n", imp.rel, strings.Join(imp.res.Sources, " + "))
	if manual == 0 {
		fmt.Fprintln(w, "  no manual action left")
	} else {
		fmt.Fprintf(w, "  %d manual action(s), also written as TODO(import) comments in the file:\n", manual)
		i := 0
		for _, n := range notes {
			if n.Severity != importer.Manual {
				continue
			}
			i++
			fmt.Fprintf(w, "  %2d. %s\n", i, strings.TrimPrefix(n.String(), "TODO "))
		}
	}
	if info := len(notes) - manual; info > 0 {
		fmt.Fprintf(w, "  %d translation choice(s) recorded as `# import:` comments in the file\n", info)
	}
}
