package repofacts

import (
	"fmt"
	"sort"
	"strings"
)

// DevConfig renders a deterministic `.sklp/dev.yaml` (kind: devstack)
// from the scanned repo. No LLM — same contract as RunnerConfig: two
// runs on the same repo produce byte-identical YAML.
//
// It composes the existing rule-based suggesters:
//   - SuggestInfraServices  → local infra (devbox/Procfile/Tiltfile)
//   - DetectMigrations      → a oneshot `migrate` service
//   - SuggestDevServices    → one service per runnable [app] module
//
// depends_on is wired conservatively: app/backend services depend on
// `migrate` when migrations exist, else on the single infra service
// when there's exactly one. Anything the rules can't infer is left out
// and surfaced as a NOTE rather than guessed.
func DevConfig(root string) (yaml string, notes []string) {
	runnables, err := ScanRunnable(root)
	if err != nil {
		notes = append(notes, "could not scan runnable modules — dev.yaml may be incomplete")
	}
	infra := SuggestInfraServices(root)
	migrations := DetectMigrations(root)
	apps := SuggestDevServices(runnables)

	// Determine the dependency anchor every app service should wait on:
	// migrate (if present) else the single infra service (if unambiguous).
	var anchor string
	switch {
	case len(migrations) > 0:
		anchor = "migrate"
	case len(infra) == 1:
		anchor = infra[0].Name
	}

	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("kind: devstack\n")
	b.WriteString("name: dev\n")
	b.WriteString("\nenv_files:\n  - .env\n")
	b.WriteString("\nservices:\n")

	wrote := 0

	// Infra first — backends depend on it.
	for _, s := range infra {
		writeDevService(&b, s)
		wrote++
	}

	// Migrations as a oneshot service depending on the infra anchor.
	for _, m := range migrations {
		svc := SuggestedService{
			Name:    "migrate",
			Run:     m.LikelyCommand,
			Workdir: m.LikelyWorkdir,
		}
		if len(infra) == 1 {
			svc.DependsOn = []string{infra[0].Name}
		}
		writeDevServiceOneshot(&b, svc)
		notes = append(notes, fmt.Sprintf("migrate service: command inferred from %s — verify `%s`", m.Source, m.LikelyCommand))
		wrote++
	}

	// App services, each anchored to migrate/infra when one exists.
	for _, s := range apps {
		if anchor != "" && !contains(s.DependsOn, anchor) && s.Name != anchor {
			s.DependsOn = append(s.DependsOn, anchor)
			sort.Strings(s.DependsOn)
		}
		writeDevService(&b, s)
		wrote++
	}

	if wrote == 0 {
		notes = append(notes, "no runnable app modules found — add at least one service by hand")
		b.WriteString("  # no app modules detected — add a service here, e.g.:\n")
		b.WriteString("  # app:\n  #   run: <your start command>\n")
	}
	return b.String(), notes
}

// writeDevService emits one long-running service block.
func writeDevService(b *strings.Builder, s SuggestedService) {
	for _, n := range s.Notes {
		fmt.Fprintf(b, "  # %s\n", n)
	}
	fmt.Fprintf(b, "  %s:\n", s.Name)
	fmt.Fprintf(b, "    run: %s\n", s.Run)
	if s.Workdir != "" {
		fmt.Fprintf(b, "    workdir: %s\n", s.Workdir)
	}
	writeDependsOn(b, s.DependsOn)
	if s.Optional {
		b.WriteString("    optional: true\n")
	}
	writeReady(b, s.Ready)
}

// writeDevServiceOneshot emits a run-to-completion service (migrations).
func writeDevServiceOneshot(b *strings.Builder, s SuggestedService) {
	fmt.Fprintf(b, "  %s:\n", s.Name)
	fmt.Fprintf(b, "    run: %s\n", s.Run)
	if s.Workdir != "" {
		fmt.Fprintf(b, "    workdir: %s\n", s.Workdir)
	}
	b.WriteString("    oneshot: true\n")
	writeDependsOn(b, s.DependsOn)
}

func writeDependsOn(b *strings.Builder, deps []string) {
	if len(deps) == 0 {
		return
	}
	b.WriteString("    depends_on:\n")
	for _, d := range deps {
		fmt.Fprintf(b, "      - %s\n", d)
	}
}

func writeReady(b *strings.Builder, r *SuggestedReady) {
	if r == nil {
		return
	}
	b.WriteString("    ready:\n")
	if r.HTTP != "" {
		fmt.Fprintf(b, "      http: %s\n", r.HTTP)
	}
	if r.TCP != "" {
		fmt.Fprintf(b, "      tcp: %s\n", r.TCP)
	}
	if r.Timeout != "" {
		fmt.Fprintf(b, "      timeout: %s\n", r.Timeout)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
