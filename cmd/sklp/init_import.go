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

// importComposeDeploy translates the repo's compose files into the stack's
// production deploy.yaml, with the same draft rule as importCompose.
func importComposeDeploy(repoAbs, stack string, force bool) (*composeImported, error) {
	runnables, _ := repofacts.ScanRunnable(repoAbs)
	res, err := importer.ImportComposeDeploy(importer.ComposeOptions{Root: repoAbs, Stack: stack, Runnables: runnables})
	if err != nil || res == nil {
		return nil, err
	}
	rel := filepath.Join(".sklp", stack, "deploy.yaml")
	if !force && fileExists(filepath.Join(repoAbs, rel)) {
		rel += ".draft"
	}
	if err := writeDraft(filepath.Join(repoAbs, rel), res.YAML, force || strings.HasSuffix(rel, ".draft")); err != nil {
		return nil, err
	}
	return &composeImported{res: res, rel: rel}, nil
}

func runK8sImport(repoAbs, stack, to, paths string, force, ascii bool) int {
	if to != "" && to != "deploy" {
		fmt.Fprintf(os.Stderr, "sklp init: -from k8s -to %q: Kubernetes describes production, the only target is deploy\n", to)
		return 2
	}
	var sources []string
	for _, p := range strings.Split(paths, ",") {
		if p = strings.TrimSpace(p); p != "" {
			sources = append(sources, p)
		}
	}
	rep := reporter.New(os.Stderr, reporter.Options{ASCII: ascii})
	rep.Header("sklp init -from k8s")
	start := time.Now()
	rep.StepStarted("importing manifests")
	res, err := importer.ImportK8sDeploy(importer.K8sOptions{Root: repoAbs, Stack: stack, Sources: sources})
	if err != nil {
		rep.StepFinished("importing manifests", reporter.StatusFailed, time.Since(start), "")
		fmt.Fprintf(os.Stderr, "sklp init: %v\n", err)
		return 1
	}
	if res == nil {
		rep.StepFinished("importing manifests", reporter.StatusFailed, time.Since(start), "")
		fmt.Fprintf(os.Stderr, "sklp init: no kustomization or Kubernetes manifests under %s (pass -path)\n", repoAbs)
		return 1
	}
	rel := filepath.Join(".sklp", stack, "deploy.yaml")
	if !force && fileExists(filepath.Join(repoAbs, rel)) {
		rel += ".draft"
	}
	if err := writeDraft(filepath.Join(repoAbs, rel), res.YAML, force || strings.HasSuffix(rel, ".draft")); err != nil {
		rep.StepFinished("importing manifests", reporter.StatusFailed, time.Since(start), "")
		fmt.Fprintf(os.Stderr, "sklp init: %v\n", err)
		return 1
	}
	imp := &composeImported{res: res, rel: rel}
	manual := importer.ManualCount(res.Notes)
	rep.StepFinished("importing manifests", reporter.StatusPassed, time.Since(start), fmt.Sprintf("%d manual action(s)", manual))
	next := fmt.Sprintf("sklp deploy apply %s -context <node>", stack)
	if manual > 0 {
		next = fmt.Sprintf("resolve TODO(import) in %s · %s", rel, next)
	}
	rep.Finalize("IMPORT", reporter.StatusPassed, time.Since(start), nil,
		[]reporter.FinalField{{Label: "next", Value: next}})
	printImportReport(os.Stderr, imp, "k8s import")
	return 0
}

func runComposeImport(repoAbs, stack, to string, force, ascii bool) int {
	importFn, verify := importCompose, fmt.Sprintf("sklp dev %s --validate", stack)
	switch to {
	case "", "dev":
	case "deploy":
		importFn, verify = importComposeDeploy, fmt.Sprintf("sklp deploy apply %s -context <node>", stack)
	default:
		fmt.Fprintf(os.Stderr, "sklp init: -to %q: want dev or deploy\n", to)
		return 2
	}
	rep := reporter.New(os.Stderr, reporter.Options{ASCII: ascii})
	rep.Header("sklp init -from compose")
	start := time.Now()
	rep.StepStarted("importing compose")
	imp, err := importFn(repoAbs, stack, force)
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
	next := strings.TrimSuffix(verify, " --validate")
	if manual > 0 {
		next = fmt.Sprintf("resolve TODO(import) in %s · %s", imp.rel, verify)
	}
	rep.Finalize("IMPORT", reporter.StatusPassed, time.Since(start), nil,
		[]reporter.FinalField{{Label: "next", Value: next}})
	printImportReport(os.Stderr, imp, "compose import")
	return 0
}

func printImportReport(w io.Writer, imp *composeImported, title string) {
	if imp == nil {
		return
	}
	notes := imp.res.Notes
	manual := importer.ManualCount(notes)
	fmt.Fprintf(w, "\n%s: %s <- %s\n", title, imp.rel, strings.Join(imp.res.Sources, " + "))
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
