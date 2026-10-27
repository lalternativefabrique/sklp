package pipeline

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// hostBinaryMatches looks for `name` on the host PATH and asks it for
// its version. When that version matches `wantVersion` on major and
// minor (patch versions are accepted as long as they are >=), it
// returns the absolute binary path and true so the caller can skip
// downloading the same tool through the native registry.
//
// Returning the host binary is opt-in conservative: we only accept a
// match on (major, minor, patch>=want). That lets a host install
// satisfy the project's pin when the host is at the same minor with
// equal or higher patch — typically what asdf/corepack set up —
// without surprising the user with a major drift.
//
// Errors from `exec.LookPath` or the probe command are NOT fatal:
// "host doesn't have it / can't parse" simply means "fall back to the
// native installer". The runner stays useful in CI containers that
// ship with nothing pre-installed.
func hostBinaryMatches(ctx context.Context, name, wantVersion string) (string, bool) {
	if wantVersion == "" {
		return "", false
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	// LookPath is happy with a symlink whose target is gone: it checks the mode
	// bits, not that the file resolves. The runner image's tools farm is exactly
	// that — /opt/sklp/tools/bin/go points into the image's own tools cache, a
	// path that does not exist once the build moved into another rootfs. Taking
	// it as the host toolchain sent every `go` invocation to a dead link, which
	// surfaced far away as "cannot find GOROOT directory".
	if _, statErr := os.Stat(path); statErr != nil {
		return "", false
	}
	if !hostToolIsUsable(ctx, name, path) {
		return "", false
	}
	gotVersion, ok := probeHostVersion(ctx, name, path)
	if !ok {
		return "", false
	}
	if !versionSatisfies(gotVersion, wantVersion) {
		return "", false
	}
	return path, true
}

// hostToolIsUsable rejects a binary that answers `--version` but cannot do its
// job. `go version` reports the version compiled into the binary and needs no
// GOROOT, so a toolchain whose GOROOT was left behind still looks healthy to a
// version probe — and is then chosen as the host tool, after which every later
// invocation dies with "cannot find GOROOT directory", far from this choice.
// `go env GOROOT` is the cheapest question that actually needs the tree.
func hostToolIsUsable(ctx context.Context, name, path string) bool {
	if name != "go" {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "env", "GOROOT").Output()
	if err != nil {
		return false
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return false
	}
	st, serr := os.Stat(root)
	return serr == nil && st.IsDir()
}

// probeHostVersion runs the per-tool version flag and returns the
// extracted semver. Each tool has its own quirk:
//   - go        prints "go version go1.25.0 linux/amd64"
//   - node      prints "v20.11.0"
//   - pnpm      prints "10.27.0"
//   - bun       prints "1.3.14"
//
// Generic fallback: the first 1.2.3-shaped substring of stdout.
func probeHostVersion(ctx context.Context, name, path string) (string, bool) {
	args := versionArgs(name)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, args...).Output()
	if err != nil {
		return "", false
	}
	// A binary that exits 0 without printing a version is not a usable tool —
	// it is something that happens to sit at that name. A CI node had a `go`
	// whose GOROOT was gone: it exited 0, printed only its complaint, and was
	// accepted as the host toolchain, so every later `go` invocation failed
	// with "cannot find GOROOT directory" far from where the choice was made.
	// Rejecting it here sends the resolver to the next rung, which installs a
	// real one.
	v := extractSemver(string(out))
	if v == "" {
		return "", false
	}
	return v, true
}

// versionArgs returns the flags `<name>` accepts to print its version.
// Go's `version` subcommand is the only odd one out among tools we
// install today.
func versionArgs(name string) []string {
	switch name {
	case "go":
		return []string{"version"}
	default:
		return []string{"--version"}
	}
}

// semverRE matches the first M.m.p triple in a string. Pre-release
// suffixes (`-rc1`, `-beta.2`) are deliberately ignored — for host
// preference we only care about the core triplet.
var semverRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// extractSemver returns the first major.minor.patch found in s, or ""
// when none matches. Cheap and good enough for the four tools we
// support today; if we ever onboard a tool that prints multiple
// version-shaped strings (`<tool> 1.0.0 built with libfoo 4.5.6`), the
// match-first policy keeps us correct for the tool itself.
func extractSemver(s string) string {
	m := semverRE.FindString(strings.TrimSpace(s))
	return m
}

// versionSatisfies reports whether `got` satisfies the pin `want`:
// same major, same minor, and patch >= want.patch.
//
// We don't accept "any major.minor >= want" because that would let a
// host pnpm 11.x silently substitute for a project pinned at 10.27.0,
// which has visible behavioural changes between majors. Same minor +
// patch-or-newer is the range where pnpm/node/go/bun commit to
// backward compatibility.
func versionSatisfies(got, want string) bool {
	gM, gm, gp, ok1 := splitSemver(got)
	wM, wm, wp, ok2 := splitSemver(want)
	if !ok1 || !ok2 {
		return false
	}
	if gM != wM || gm != wm {
		return false
	}
	return gp >= wp
}

// splitSemver parses "a.b.c" into ints. Anything else returns
// (0, 0, 0, false). We tolerate `v` prefixes (node prints `v20.11.0`).
func splitSemver(s string) (int, int, int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	m := semverRE.FindStringSubmatch(s)
	if len(m) != 4 {
		return 0, 0, 0, false
	}
	a, err1 := atoi(m[1])
	b, err2 := atoi(m[2])
	c, err3 := atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return a, b, c, true
}

// atoi is strconv.Atoi behind a local name so the call sites stay
// short. Inlined error propagation in splitSemver is intentional —
// folding three Atois through a helper hides the failure point.
func atoi(s string) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errBadSemver
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errBadSemver = stringError("not a base-10 integer")

type stringError string

func (e stringError) Error() string { return string(e) }
