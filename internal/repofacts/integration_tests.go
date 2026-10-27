package repofacts

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// knownInfraServices is the set of substrings the integration-tests
// detector recognises as "an infra dependency". The list is
// deliberately short: each entry must map to a service we can stand
// up via compose / sklp `services:` block in CI. Variants (postgres
// vs postgresql) are normalised by infraServiceName below.
var knownInfraServices = []string{
	"postgresql", "postgres", "redis", "nats",
	"clickhouse", "mongodb", "mongo", "mysql", "mariadb",
}

// isComposeFile recognises every compose filename variant we want
// to surface in RepoFacts.ComposeFiles. The CI generator needs
// compose.test.yml / compose.ci.yml in particular, since teams
// keep test-specific (lighter, ephemeral) infra there.
func isComposeFile(base string) bool {
	switch base {
	case "compose.yml", "compose.yaml",
		"docker-compose.yml", "docker-compose.yaml":
		return true
	}
	// compose.<variant>.yml / docker-compose.<variant>.yml
	for _, prefix := range []string{"compose.", "docker-compose."} {
		if !strings.HasPrefix(base, prefix) {
			continue
		}
		rest := base[len(prefix):]
		if strings.HasSuffix(rest, ".yml") || strings.HasSuffix(rest, ".yaml") {
			return true
		}
	}
	return false
}

// detectIntegrationTests scans the file list for evidence of
// infra-dependent integration tests. The heuristics are file-name
// based first (`*_integration_test.go`) and content-based second
// (`//go:build integration`, mentions of `postgres`/`nats`/...).
//
// It also consults composeFiles to widen the service-hint list:
// if a compose.test.yml mentions a service we know, we surface it
// even when no Go file does. That's the Acme pattern.
func detectIntegrationTests(walked []string, root string, composeFiles []string) IntegrationTestFacts {
	var out IntegrationTestFacts
	hints := map[string]struct{}{}

	for _, p := range walked {
		base := filepath.Base(p)
		if !strings.HasSuffix(base, "_test.go") {
			continue
		}
		// Two conventions are widespread:
		//   - `integration_test.go` (the file name == "integration_test.go")
		//   - `<svc>_integration_test.go` (a per-area file)
		// Match both.
		isIntegrationByName := base == "integration_test.go" ||
			strings.HasSuffix(base, "_integration_test.go")
		tag, services := peekGoTestFile(p)
		for _, s := range services {
			hints[s] = struct{}{}
		}
		if tag {
			out.HasBuildTag = true
		}
		if isIntegrationByName || tag {
			if len(out.GoFiles) < 20 {
				out.GoFiles = append(out.GoFiles, rel(root, p))
			}
		}
	}

	// Compose files contribute service hints too. We don't parse the
	// YAML; a substring match against known service names is enough
	// for the CI prompt's purposes (it'll re-read the file via
	// read_file to extract the actual image/ports/env).
	for _, cf := range composeFiles {
		abs := filepath.Join(root, cf)
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		lower := strings.ToLower(string(data))
		for _, svc := range knownInfraServices {
			if strings.Contains(lower, svc) {
				hints[infraServiceName(svc)] = struct{}{}
			}
		}
	}

	if len(hints) > 0 {
		out.ServiceHints = make([]string, 0, len(hints))
		for s := range hints {
			out.ServiceHints = append(out.ServiceHints, s)
		}
		sort.Strings(out.ServiceHints)
	}

	sort.Strings(out.GoFiles)
	return out
}

// peekGoTestFile inspects up to the first 100 lines of a Go test
// file for an `integration` build tag and any mention of known
// infra service names. Bounded reads keep the cost predictable
// even on a repo with hundreds of test files.
func peekGoTestFile(path string) (hasIntegrationTag bool, services []string) {
	f, err := os.Open(path)
	if err != nil {
		return false, nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	seen := map[string]struct{}{}
	lines := 0
	sawCode := false
	for scanner.Scan() {
		line := scanner.Text()
		lines++
		if lines > 200 {
			break
		}
		trim := strings.TrimSpace(line)
		// Build tag detection — must come before the `package` line.
		if !sawCode {
			if strings.HasPrefix(trim, "//go:build") && strings.Contains(trim, "integration") {
				hasIntegrationTag = true
			}
			if strings.HasPrefix(trim, "// +build") && strings.Contains(trim, "integration") {
				hasIntegrationTag = true
			}
			if strings.HasPrefix(trim, "package ") {
				sawCode = true
			}
		}
		lower := strings.ToLower(line)
		for _, svc := range knownInfraServices {
			if strings.Contains(lower, svc) {
				seen[infraServiceName(svc)] = struct{}{}
			}
		}
	}
	for s := range seen {
		services = append(services, s)
	}
	return hasIntegrationTag, services
}

// infraServiceName normalises a raw service hint into the canonical
// short name we want to see in the generated `services:` list.
// (postgresql → postgres, mongodb → mongo).
func infraServiceName(s string) string {
	switch s {
	case "postgresql":
		return "postgres"
	case "mongodb":
		return "mongo"
	default:
		return s
	}
}
