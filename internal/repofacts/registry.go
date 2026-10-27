package repofacts

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// detectRegistry tries to infer the container registry root from
// signals in the repo. Returns "" when no signal is strong enough —
// the caller surfaces a placeholder + NOTE rather than guessing.
//
// Precedence (first match wins):
//
//  1. .sklp/config.yaml `runner.registry: <url>` — the operator's
//     explicit declaration; most authoritative.
//  2. Makefile `REGISTRY ?= <url>` or `REGISTRY := <url>` line.
//  3. docker-compose images that share a common prefix (e.g.
//     ghcr.io/myorg/api + ghcr.io/myorg/web → ghcr.io/myorg).
//  4. GitHub Actions workflow with a `docker/build-push-action` that
//     tags `ghcr.io/${{ github.repository }}` — we resolve to
//     ghcr.io/<owner>.
//
// This list is conservative on purpose. False positives are worse
// than empty: a wrong registry in a publish.yaml.draft is a silent
// publish to the wrong place.
func detectRegistry(root string, mk *MakefileFacts) string {
	if r := detectRegistryFromSklpConfig(root); r != "" {
		return r
	}
	if mk != nil {
		if r := detectRegistryFromMakefile(filepath.Join(root, mk.Path)); r != "" {
			return r
		}
	}
	if r := detectRegistryFromCompose(root); r != "" {
		return r
	}
	return ""
}

// sklpRegistryRe matches the `registry:` key under the `runner:` block
// of .sklp/space.yaml. We don't full-parse the YAML — a line regex is
// enough since the key is always two-space indented under runner: and
// the value is a bare URL. A placeholder value (`<your-namespace>`) is
// rejected by the caller via the angle-bracket check below.
var sklpRegistryRe = regexp.MustCompile(`(?m)^\s+registry:\s*([^\s#]+)`)

// detectRegistryFromSklpConfig reads the space recipe (space.yaml, or the
// legacy config.yaml) and returns the runner registry, skipping the
// placeholder that `sklp init` writes when it couldn't infer one (so a
// placeholder doesn't get promoted into a "real" inferred value). This is a
// read-only detection path, so it does NOT auto-migrate the legacy file —
// only the recipe-loading path does that.
func detectRegistryFromSklpConfig(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".sklp", "space.yaml"))
	if err != nil {
		raw, err = os.ReadFile(filepath.Join(root, ".sklp", "config.yaml"))
	}
	if err != nil {
		return ""
	}
	m := sklpRegistryRe.FindSubmatch(raw)
	if m == nil {
		return ""
	}
	v := strings.TrimRight(strings.Trim(strings.TrimSpace(string(m[1])), `"'`), "/")
	// Reject the angle-bracket placeholder (<your-namespace>).
	if strings.ContainsAny(v, "<>") {
		return ""
	}
	return v
}

// registryAssignRe matches `REGISTRY ?= <value>` or `REGISTRY := <value>`
// or `REGISTRY = <value>` in a Makefile, capturing the value with
// surrounding whitespace trimmed.
var registryAssignRe = regexp.MustCompile(`(?m)^REGISTRY\s*[?:]?=\s*([^\s#]+)`)

// detectRegistryFromMakefile scans the Makefile line-by-line for the
// REGISTRY variable assignment. We do not evaluate make functions or
// includes — if the user set REGISTRY in a non-trivial way, they can
// override the result of the LLM later.
func detectRegistryFromMakefile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if m := registryAssignRe.FindSubmatch(raw); m != nil {
		v := strings.TrimSpace(string(m[1]))
		// Strip trailing slash so consumers can concatenate cleanly.
		v = strings.TrimRight(v, "/")
		return v
	}
	return ""
}

// detectRegistryFromCompose reads the first compose file we find and
// extracts a common prefix from the `image:` fields. It's a fallback,
// not a primary source — only used when Makefile didn't yield a hit.
func detectRegistryFromCompose(root string) string {
	for _, name := range []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"} {
		path := filepath.Join(root, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		images := readComposeImages(f)
		_ = f.Close()
		if prefix := longestCommonImagePrefix(images); prefix != "" {
			return prefix
		}
	}
	return ""
}

// composeImageLineRe matches `image: <ref>` lines in a compose file.
// The capture is greedy up to whitespace or end-of-line; tags after
// `:` are stripped by the caller before computing common prefix.
var composeImageLineRe = regexp.MustCompile(`(?m)^\s*image:\s*([^\s#]+)`)

// readComposeImages returns every `image:` value from a compose file.
// We do not parse YAML — a regex over the line is enough because
// compose files are flat enough that no quoting ambiguity hurts.
func readComposeImages(f *os.File) []string {
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var content strings.Builder
	for sc.Scan() {
		content.WriteString(sc.Text())
		content.WriteByte('\n')
	}
	for _, m := range composeImageLineRe.FindAllStringSubmatch(content.String(), -1) {
		img := strings.Trim(m[1], `"'`)
		// Drop the tag — we only want the registry path.
		if i := strings.LastIndex(img, ":"); i >= 0 {
			// Make sure we don't truncate `registry.host:port/...`.
			rest := img[i+1:]
			if !strings.Contains(rest, "/") {
				img = img[:i]
			}
		}
		if img != "" {
			out = append(out, img)
		}
	}
	return out
}

// longestCommonImagePrefix returns the longest `<host>[/<ns>]`
// shared by all images, or "" when they don't share at least two
// path segments. Two segments because `ghcr.io/foo/bar` and
// `ghcr.io/foo/baz` should yield `ghcr.io/foo`, not `ghcr.io`.
func longestCommonImagePrefix(imgs []string) string {
	if len(imgs) < 2 {
		return ""
	}
	// Reuse the first image's segments as the running prefix and
	// shrink it as we visit subsequent images.
	parts := strings.Split(imgs[0], "/")
	for _, img := range imgs[1:] {
		other := strings.Split(img, "/")
		n := len(parts)
		if len(other) < n {
			n = len(other)
		}
		i := 0
		for ; i < n; i++ {
			if parts[i] != other[i] {
				break
			}
		}
		parts = parts[:i]
		if len(parts) < 2 {
			return ""
		}
	}
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts, "/")
}
