package pipeline

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// WireGitHubToken makes private github.com modules fetchable by
// rewriting github URLs to a token-bearing HTTPS URL.
//
// WHERE it writes that rewrite depends on who owns the HOME it would write to:
//
//   - Inside a runner sandbox, ~/.gitconfig is throwaway, and a global write is
//     the only thing that reaches every git invocation a step makes, including
//     the ones Go spawns for `go mod download`.
//   - On a developer's machine it is NOT throwaway. A global write edits the
//     user's own config, and the rewrite OUTLIVES the run: the token expires,
//     but the rule stays and silently redirects every future github operation
//     to a dead credential — including plain `git push`, which then fails with
//     "Invalid username or token" and no hint as to why.
//
// So off-sandbox the run gets a global config of its own through
// GIT_CONFIG_GLOBAL: the user's settings minus their url rewrites, plus the
// token one. A developer's own `https://github.com/ → git@github.com:` rewrite
// shares the token rule's prefix, and git settles that tie on the rule it read
// first, so a rule merely appended through GIT_CONFIG_COUNT never won against
// ~/.gitconfig: the fetch went over ssh, to a host with no key.
func WireGitHubToken(stderr io.Writer) error {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil
	}
	rewriteTo := fmt.Sprintf("https://oauth2:%s@github.com/", token)
	key := fmt.Sprintf("url.%s.insteadOf", rewriteTo)

	// Rewrite BOTH HTTPS and SSH forms: a private module pulled via
	// `git@github.com:` (a go.mod replace, a .gitmodules SSH URL) would
	// otherwise bypass an HTTPS-only rewrite and clone over ssh, which the
	// runner has no key for ("could not read Username" / host-key prompt).
	froms := []string{"https://github.com/", "ssh://git@github.com/", "git@github.com:"}

	if alreadyInSandbox() {
		// The key holds MULTIPLE values (one per `from`), so they must be
		// --add'ed, not --replace-all'd (a single --replace-all collapses the
		// key to one value). WireGitHubToken runs twice per pipeline (once
		// early in Run() with the shell token, once post-env_files with the
		// resolved token). To stay idempotent, drop any prior values for this
		// exact key first, then re-add the full set. unset-all on an absent
		// key exits 5 — treated as a no-op.
		_ = runGit("config", "--global", "--unset-all", key)
		for _, from := range froms {
			if err := runGit("config", "--global", "--add", key, from); err != nil {
				// Never surface key/err verbatim: both embed the PAT. runGit
				// already redacts its captured output, but the args we passed
				// (key) carry the token too, so report the operation, not the
				// command.
				return fmt.Errorf("git config insteadOf: %w", err)
			}
		}
	} else if err := exportRunGitConfig(key, froms); err != nil {
		return err
	}

	if os.Getenv("GOPRIVATE") == "" {
		_ = os.Setenv("GOPRIVATE", "github.com/*")
	}
	if stderr != nil {
		scope := "env"
		if alreadyInSandbox() {
			scope = "global"
		}
		fmt.Fprintf(stderr, "[sklp] GITHUB_TOKEN wired (%s, GOPRIVATE=%s)\n", scope, os.Getenv("GOPRIVATE"))
	}
	return nil
}

// exportRunGitConfig writes the run's global git config and points
// GIT_CONFIG_GLOBAL at it. It is rebuilt from the user's real global config on
// every call, so the second wiring of a run sees the resolved token, not the
// file the first one wrote.
func exportRunGitConfig(key string, froms []string) error {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("sklp-gitconfig-%d", os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create run git config: %w", err)
	}
	f.Close()
	for _, kv := range userGitConfig() {
		k, v, _ := strings.Cut(kv, "=")
		if isURLRewrite(k) {
			continue
		}
		if err := runGitFile(path, "--add", k, v); err != nil {
			return err
		}
	}
	for _, from := range froms {
		if err := runGitFile(path, "--add", key, from); err != nil {
			return fmt.Errorf("git config insteadOf: %w", err)
		}
	}
	return os.Setenv("GIT_CONFIG_GLOBAL", path)
}

// userGitConfig lists the user's own global config as key=value lines. The
// inherited GIT_CONFIG_GLOBAL is dropped so a rewire reads ~/.gitconfig, not
// the file a previous wiring produced.
func userGitConfig() []string {
	cmd := exec.Command("git", "config", "--global", "--list")
	cmd.Env = append(stripGitVars(os.Environ()), "GIT_DIR=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

func isURLRewrite(key string) bool {
	k := strings.ToLower(key)
	return strings.HasPrefix(k, "url.") && (strings.HasSuffix(k, ".insteadof") || strings.HasSuffix(k, ".pushinsteadof"))
}

func runGitFile(path string, args ...string) error {
	cmd := exec.Command("git", append([]string{"config", "-f", path}, args...)...)
	cmd.Env = append(stripGitVars(os.Environ()), "GIT_DIR=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git config failed: %w (%s)", err, redactToken(strings.TrimSpace(string(out))))
	}
	return nil
}

// MarkWorkspaceSafe registers repoPath as a git safe.directory, skipping the
// write when it is already registered: safe.directory holds multiple values and
// --add appends unconditionally, so a plain add grows the user's ~/.gitconfig by
// one duplicate line on every single run.
func MarkWorkspaceSafe(repoPath string, stderr io.Writer) error {
	if repoPath == "" {
		return nil
	}
	if gitConfigHasValue("safe.directory", repoPath) {
		return nil
	}
	if err := runGit("config", "--global", "--add", "safe.directory", repoPath); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "[sklp] warning: could not mark %s as safe.directory: %v\n", repoPath, err)
		}
		return nil
	}
	return nil
}

// gitConfigHasValue reports whether key already holds value in the global git
// config. It resolves no repository, for the reason runGit documents.
func gitConfigHasValue(key, value string) bool {
	cmd := exec.Command("git", "config", "--global", "--get-all", key)
	cmd.Env = append(stripGitVars(os.Environ()), "GIT_DIR=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == value {
			return true
		}
	}
	return false
}

// runGit runs `git` for repo-independent global config writes (insteadOf,
// safe.directory). These touch only $HOME/.gitconfig, so they must NOT inherit
// the caller's repository context: a linked worktree exports GIT_DIR /
// GIT_WORK_TREE / GIT_COMMON_DIR (or ships a `.git` file whose `gitdir:` points
// at a host path absent inside the container), and `git config --global` then
// aborts with "not a git repository" (exit 128) before it ever reaches HOME.
// We strip every GIT_* var and pin GIT_DIR=/dev/null so git resolves no repo.
func runGit(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Env = append(stripGitVars(os.Environ()), "GIT_DIR=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git config failed: %w (%s)", err, redactToken(strings.TrimSpace(string(out))))
	}
	return nil
}

// stripGitVars drops every GIT_* entry from a KEY=VALUE env slice so a git
// invocation resolves no inherited repository.
func stripGitVars(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// tokenPat matches the credential shapes that can appear in a failed git
// command echo: oauth2:<tok>@ / x-access-token:<tok>@ userinfo, and GitHub
// PAT prefixes (ghp_, gho_, ghs_, ghu_, ghr_, github_pat_).
var tokenPat = regexp.MustCompile(`(oauth2|x-access-token):[^@/\s]+@|gh[pousr]_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+`)

// redactToken masks any embedded credential so a failed git command never
// leaks the PAT into logs.
func redactToken(s string) string {
	return tokenPat.ReplaceAllStringFunc(s, func(m string) string {
		if i := strings.IndexByte(m, ':'); i >= 0 && strings.HasSuffix(m, "@") {
			return m[:i+1] + "***@"
		}
		return "***"
	})
}
