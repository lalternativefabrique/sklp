package pipeline

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
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
// So off-sandbox we pass the same rewrite through GIT_CONFIG_COUNT/KEY/VALUE
// instead. Git applies it to every invocation that inherits this environment
// and to nothing else, so it disappears when the run does.
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
	} else if err := exportGitConfigEnv(key, froms); err != nil {
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

// exportGitConfigEnv appends one key/value pair per `from` to the process
// environment using git's GIT_CONFIG_COUNT/KEY_n/VALUE_n protocol, preserving
// any entries a caller already set.
func exportGitConfigEnv(key string, froms []string) error {
	count := 0
	if existing := os.Getenv("GIT_CONFIG_COUNT"); existing != "" {
		n, err := strconv.Atoi(existing)
		if err != nil {
			return fmt.Errorf("GIT_CONFIG_COUNT is not a number: %q", existing)
		}
		count = n
	}
	for _, from := range froms {
		if err := os.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", count), key); err != nil {
			return fmt.Errorf("export git config key: %w", err)
		}
		if err := os.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", count), from); err != nil {
			return fmt.Errorf("export git config value: %w", err)
		}
		count++
	}
	return os.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(count))
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
