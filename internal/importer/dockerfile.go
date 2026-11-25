package importer

import (
	"os"
	"regexp"
	"strings"
)

type dockerfileFacts struct {
	text       string
	finalFrom  string
	stages     []string
	aptPkgs    []string
	apkPkgs    []string
	copySource []string
}

var (
	fromRE    = regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	aptRE     = regexp.MustCompile(`apt-get\s+(?:-\S+\s+)*install\s+([^&;|]+)`)
	apkRE     = regexp.MustCompile(`apk\s+(?:-\S+\s+)*add\s+([^&;|]+)`)
	copyRE    = regexp.MustCompile(`(?i)^(?:COPY|ADD)\s+(.+)$`)
	pkgWordRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.+_-]*(?:=[^\s]+)?$`)
)

func readDockerfile(path string) (dockerfileFacts, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dockerfileFacts{}, false
	}
	f := dockerfileFacts{text: string(data)}
	for _, line := range joinContinuations(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := fromRE.FindStringSubmatch(line); m != nil {
			f.finalFrom = m[1]
			f.copySource = nil
			if m[2] != "" {
				f.stages = append(f.stages, m[2])
			}
			continue
		}
		if m := copyRE.FindStringSubmatch(line); m != nil {
			if !strings.Contains(m[1], "--from=") {
				fields := strings.Fields(m[1])
				for _, src := range fields[:max(0, len(fields)-1)] {
					if !strings.HasPrefix(src, "--") {
						f.copySource = append(f.copySource, src)
					}
				}
			}
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(line), "RUN") {
			continue
		}
		for _, m := range aptRE.FindAllStringSubmatch(line, -1) {
			f.aptPkgs = append(f.aptPkgs, packageWords(m[1])...)
		}
		for _, m := range apkRE.FindAllStringSubmatch(line, -1) {
			f.apkPkgs = append(f.apkPkgs, packageWords(m[1])...)
		}
	}
	f.aptPkgs = uniqueSorted(f.aptPkgs)
	f.apkPkgs = uniqueSorted(f.apkPkgs)
	return f, true
}

func packageWords(s string) []string {
	var out []string
	for _, w := range strings.Fields(s) {
		if strings.HasPrefix(w, "-") || !pkgWordRE.MatchString(w) {
			continue
		}
		name, _, _ := strings.Cut(w, "=")
		out = append(out, name)
	}
	return out
}

func joinContinuations(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteByte(' ')
			continue
		}
		cur.WriteString(trimmed)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// referencedModules returns the module dirs a Dockerfile names in its COPY,
// WORKDIR or RUN lines, which tells which app a monorepo-root build packages.
func (f dockerfileFacts) referencedModules(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
		re := regexp.MustCompile(`(?m)(^|[\s/"'=])` + regexp.QuoteMeta(d) + `($|[\s/"'])`)
		if re.MatchString(f.text) {
			out = append(out, d)
		}
	}
	return out
}
