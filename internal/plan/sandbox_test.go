package plan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestComposeEnv_AllowlistDropsHostVars(t *testing.T) {
	host := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/u",
		"AWS_SECRET_ACCESS_KEY=leak-me",
		"GITHUB_TOKEN=tok",
		"LANG=en_US.UTF-8",
		"BIZARRE_VAR=present",
	}
	t.Setenv(cpuBudgetEnv, "2")
	got := composeEnv(sandboxSpec{HostEnv: host})

	want := map[string]string{
		"PATH":       "/usr/bin:/bin",
		"HOME":       "/home/u",
		"LANG":       "en_US.UTF-8",
		"GOMAXPROCS": "2",
		"GOFLAGS":    "-mod=readonly",
	}
	gotMap := envToMap(got)
	if !reflect.DeepEqual(gotMap, want) {
		t.Fatalf("composeEnv = %v, want %v", gotMap, want)
	}
}

func TestComposeEnv_StepEnvWinsOverAllowlist(t *testing.T) {
	host := []string{"PATH=/usr/bin", "HOME=/h"}
	got := composeEnv(sandboxSpec{
		HostEnv: host,
		StepEnv: map[string]string{"PATH": "/opt/bin:/usr/bin", "CI": "true"},
	})
	m := envToMap(got)
	if m["PATH"] != "/opt/bin:/usr/bin" {
		t.Errorf("PATH not overridden: %q", m["PATH"])
	}
	if m["CI"] != "true" {
		t.Errorf("CI not set: %q", m["CI"])
	}
}

func TestComposeEnv_ToolDirsPrepended(t *testing.T) {
	host := []string{"PATH=/usr/bin"}
	got := composeEnv(sandboxSpec{
		HostEnv:  host,
		ToolDirs: []string{"/opt/go/bin", "/opt/node/bin"},
	})
	m := envToMap(got)
	if m["PATH"] != "/opt/go/bin:/opt/node/bin:/usr/bin" {
		t.Errorf("tool dirs not prepended: %q", m["PATH"])
	}
}

func TestComposeEnv_SecretsOptIn(t *testing.T) {
	host := []string{"PATH=/usr/bin"}
	got := composeEnv(sandboxSpec{
		HostEnv:   host,
		Secrets:   []string{"GITHUB_TOKEN"},
		AllSecret: map[string]string{"GITHUB_TOKEN": "tok", "AWS_KEY": "nope"},
	})
	m := envToMap(got)
	if m["GITHUB_TOKEN"] != "tok" {
		t.Errorf("opted-in secret missing: %v", m)
	}
	if _, ok := m["AWS_KEY"]; ok {
		t.Errorf("non-opted secret leaked: %v", m)
	}
}

func TestComposeEnv_SecretsWinOverStepEnv(t *testing.T) {
	got := composeEnv(sandboxSpec{
		HostEnv:   []string{"PATH=/usr/bin"},
		StepEnv:   map[string]string{"GITHUB_TOKEN": "fake"},
		Secrets:   []string{"GITHUB_TOKEN"},
		AllSecret: map[string]string{"GITHUB_TOKEN": "real"},
	})
	if envToMap(got)["GITHUB_TOKEN"] != "real" {
		t.Fatalf("secret should win over step env")
	}
}

func TestResolveSandboxCwd(t *testing.T) {
	repo := t.TempDir()
	sub := filepath.Join(repo, "apps", "core")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSandboxCwd(repo, "")
	if err != nil || got != repo {
		t.Errorf("empty cwd: got %q err %v", got, err)
	}

	got, err = resolveSandboxCwd(repo, "apps/core")
	if err != nil || got != sub {
		t.Errorf("subdir cwd: got %q err %v", got, err)
	}

	if _, err := resolveSandboxCwd(repo, "../etc"); err == nil {
		t.Errorf("escape via .. should fail")
	}
}

func TestCleanStepCwd_Escape(t *testing.T) {
	cases := []struct {
		in   string
		fail bool
	}{
		{"", false},
		{".", false},
		{"apps/core", false},
		{"./apps/core", false},
		{"..", true},
		{"../etc", true},
		{"apps/../../etc", true},
		{"/etc/passwd", true},
	}
	for _, c := range cases {
		_, err := cleanStepCwd(c.in)
		if (err != nil) != c.fail {
			t.Errorf("cleanStepCwd(%q): err=%v fail=%v", c.in, err, c.fail)
		}
	}
}

func TestCompileCI_RejectsCwdEscape(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "bad", Run: "true", Cwd: "../../etc"},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
	if ce.Step != "bad" {
		t.Errorf("step = %q", ce.Step)
	}
}

func TestCompileCI_RejectsUnknownSecret(t *testing.T) {
	in := CompileCIInput{
		Secrets: []string{"GH_TOKEN"},
		Steps: []CIStepInput{
			{Name: "leak", Run: "true", Secrets: []string{"AWS_KEY"}},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
}

func TestExecuteCI_AppliesAllowlist(t *testing.T) {
	// A step running `env` must not see a non-allowlisted host var.
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "env", Run: "env", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	host := append(os.Environ(), "SKLP_TEST_LEAK=should-not-see")
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), BaseEnv: host,
		Stdout: &out, Stderr: &out,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.PassFail != "pass" {
		t.Fatalf("pass_fail=%q out=%s", rep.PassFail, out.String())
	}
	if strings.Contains(out.String(), "SKLP_TEST_LEAK") {
		t.Errorf("non-allowlisted var leaked into step env:\n%s", out.String())
	}
}

func TestExecuteCI_RunsAtStepCwd(t *testing.T) {
	repo := t.TempDir()
	sub := filepath.Join(repo, "child")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{Name: "pwd", Run: "pwd", Cwd: "child", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: repo, BaseEnv: os.Environ(),
		Stdout: &out, Stderr: &out,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	// pwd may resolve symlinks (/tmp -> /private/tmp on macOS); compare basenames.
	got := strings.TrimSpace(out.String())
	if filepath.Base(got) != "child" {
		t.Errorf("pwd = %q, want basename 'child'", got)
	}
}

func TestExecuteCI_OptInSecretReachesStep(t *testing.T) {
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Secrets: []string{"GH_TOKEN"},
			Steps: []CIStep{
				{Name: "ok", Run: "echo TOKEN=$GH_TOKEN", Status: StatusIncluded, Secrets: []string{"GH_TOKEN"}},
				{Name: "no", Run: "echo NO=$GH_TOKEN", Status: StatusIncluded},
			},
		},
	}
	var out bytes.Buffer
	if _, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), BaseEnv: os.Environ(),
		Secrets: map[string]string{"GH_TOKEN": "s3cret"},
		Stdout:  &out, Stderr: &out,
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "TOKEN=s3cret") {
		t.Errorf("opted-in step did not see secret: %q", s)
	}
	if strings.Contains(s, "NO=s3cret") {
		t.Errorf("non-opted step saw secret: %q", s)
	}
}

func TestExecuteCI_CancelKillsChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix only")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				// Spawn a child sleep, write its PID to the marker
				// file, then wait. We assert the child PID is gone
				// after the ctx cancel propagates SIGTERM/SIGKILL.
				{
					Name:   "spawn",
					Run:    "sleep 30 & echo $! > " + marker + "; wait",
					Status: StatusIncluded,
				},
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	doneCh := make(chan error, 1)
	go func() {
		_, err := ExecuteCI(ctx, CIExecConfig{
			Plan: pl, Repo: dir, BaseEnv: os.Environ(),
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		})
		doneCh <- err
	}()

	// Wait for the marker file to appear, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-doneCh
			t.Fatal("marker file not created in time")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pidBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(pidBytes))

	cancel()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteCI did not return after cancel")
	}

	// Verify the child sleep is gone. Allow a short window for init
	// to reap the orphaned zombie.
	deadline = time.Now().Add(5 * time.Second)
	for !pidGone(pid) {
		if time.Now().After(deadline) {
			t.Errorf("child process %s still alive after cancel", pid)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func pidGone(pid string) bool {
	if pid == "" {
		return true
	}
	n, err := strconv.Atoi(pid)
	if err != nil {
		return true
	}
	// On Linux check /proc — a reaped or dead-non-zombie pid has no
	// entry there. Treat a zombie (state Z) as "gone for our purposes":
	// the process has terminated, it just hasn't been reaped yet by
	// its parent (or by PID 1 if orphaned). In a Pod where the runner
	// is PID 1 with no explicit reaper, zombies can linger arbitrarily
	// — but the kill side of cancellation has done its job. kill(pid,
	// 0) returning ESRCH is the POSIX backstop for non-Linux.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/proc/" + pid); os.IsNotExist(err) {
			return true
		}
		if statBytes, err := os.ReadFile("/proc/" + pid + "/stat"); err == nil {
			// /proc/<pid>/stat format: "pid (comm) state ppid ..." —
			// comm can contain spaces and parens, so we anchor on the
			// last ")" and read the next field as the state char.
			s := string(statBytes)
			if i := strings.LastIndex(s, ")"); i != -1 && i+2 < len(s) {
				if state := s[i+2]; state == 'Z' || state == 'X' {
					return true
				}
			}
		}
	}
	err = syscall.Kill(n, 0)
	return errors.Is(err, syscall.ESRCH)
}

func envToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		out[kv[:eq]] = kv[eq+1:]
	}
	return out
}

// TestIsGitWorktree distinguishes the three repo states sklp can land in:
// regular checkout (`.git` is a dir), worktree (`.git` is a file), and
// no git at all (no `.git` entry). Only the worktree case must trigger
// the GOFLAGS workaround.
func TestIsGitWorktree(t *testing.T) {
	t.Run("regular checkout returns false", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if isGitWorktree(dir) {
			t.Error("expected false for `.git` directory")
		}
	})
	t.Run("worktree returns true", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !isGitWorktree(dir) {
			t.Error("expected true for `.git` regular file")
		}
	})
	t.Run("no git returns false", func(t *testing.T) {
		if isGitWorktree(t.TempDir()) {
			t.Error("expected false when `.git` is absent")
		}
	})
	t.Run("empty repo returns false", func(t *testing.T) {
		if isGitWorktree("") {
			t.Error("expected false for empty repo path")
		}
	})
}

// TestComposeEnv_WorktreeInjectsGoflags pins the worktree workaround:
// when Repo points at a `.git`-as-file (worktree), the sandbox must
// inject `-buildvcs=false` into GOFLAGS so go-toolchain commands stop
// failing with "error obtaining VCS status".
func TestComposeEnv_WorktreeInjectsGoflags(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := envToMap(composeEnv(sandboxSpec{Repo: dir}))
	const want = "-mod=readonly -buildvcs=false"
	if got["GOFLAGS"] != want {
		t.Errorf("GOFLAGS = %q, want %q", got["GOFLAGS"], want)
	}
}

// TestComposeEnv_RegularCheckoutLeavesGoflagsAlone confirms the
// buildvcs workaround is scoped: a normal checkout (production CI
// clone) must not see `-buildvcs=false` injected, so binaries keep
// their vcs.revision stamp. `-mod=readonly` is unconditional and stays.
func TestComposeEnv_RegularCheckoutLeavesGoflagsAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := envToMap(composeEnv(sandboxSpec{Repo: dir}))
	if strings.Contains(got["GOFLAGS"], "-buildvcs=") {
		t.Errorf("buildvcs unexpectedly set on regular checkout: %q", got["GOFLAGS"])
	}
}

// TestComposeEnv_UserGoflagsPreserved ensures that when the user (or a
// pipeline) explicitly sets -buildvcs in GOFLAGS, the sandbox respects
// it instead of clobbering. Covers both `-buildvcs=true` (deliberate
// opt-in despite the worktree bug) and `-buildvcs=false` (already
// applied — must not double up).
func TestComposeEnv_UserGoflagsPreserved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, userGoflags, want string
	}{
		{"explicit true respected", "-buildvcs=true", "-buildvcs=true -mod=readonly"},
		{"explicit false not doubled", "-buildvcs=false -mod=vendor", "-buildvcs=false -mod=vendor"},
		{"unrelated GOFLAGS gets buildvcs appended", "-mod=vendor", "-mod=vendor -buildvcs=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := envToMap(composeEnv(sandboxSpec{
				Repo:    dir,
				StepEnv: map[string]string{"GOFLAGS": tc.userGoflags},
			}))
			if got["GOFLAGS"] != tc.want {
				t.Errorf("GOFLAGS = %q, want %q", got["GOFLAGS"], tc.want)
			}
		})
	}
}

// TestComposeEnv_AlwaysDeclaresSafeDirectory pins the hosted-CI fix:
// the sandbox always tells git to treat the repo as a safe directory,
// via the GIT_CONFIG_COUNT/KEY_n/VALUE_n protocol. This must apply to
// both regular checkouts and worktrees — the failure mode (UID
// mismatch in a container) has nothing to do with the worktree flag.
func TestComposeEnv_AlwaysDeclaresSafeDirectory(t *testing.T) {
	dir := t.TempDir()
	got := envToMap(composeEnv(sandboxSpec{Repo: dir}))
	if got["GIT_CONFIG_COUNT"] != "1" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want %q", got["GIT_CONFIG_COUNT"], "1")
	}
	if got["GIT_CONFIG_KEY_0"] != "safe.directory" {
		t.Errorf("GIT_CONFIG_KEY_0 = %q, want safe.directory", got["GIT_CONFIG_KEY_0"])
	}
	if got["GIT_CONFIG_VALUE_0"] != dir {
		t.Errorf("GIT_CONFIG_VALUE_0 = %q, want %q", got["GIT_CONFIG_VALUE_0"], dir)
	}
}

// TestComposeEnv_SafeDirectorySkippedWhenNoRepo confirms the guard:
// without a repo path (e.g. ad-hoc invocations, tests) the sandbox
// must not invent a safe.directory entry.
func TestComposeEnv_SafeDirectorySkippedWhenNoRepo(t *testing.T) {
	got := envToMap(composeEnv(sandboxSpec{}))
	if _, ok := got["GIT_CONFIG_COUNT"]; ok {
		t.Errorf("GIT_CONFIG_COUNT unexpectedly set: %v", got)
	}
}

// TestComposeEnv_SafeDirectoryMergesUserGitConfig covers the
// concurrent-config case: when the caller already declared one or
// more GIT_CONFIG_KEY_n/VALUE_n pairs (e.g. a pipeline that opts into
// a credential helper), our safe.directory entry must be appended
// after theirs without breaking the count.
func TestComposeEnv_SafeDirectoryMergesUserGitConfig(t *testing.T) {
	dir := t.TempDir()
	got := envToMap(composeEnv(sandboxSpec{
		Repo: dir,
		StepEnv: map[string]string{
			"GIT_CONFIG_COUNT":   "1",
			"GIT_CONFIG_KEY_0":   "credential.helper",
			"GIT_CONFIG_VALUE_0": "store",
		},
	}))
	if got["GIT_CONFIG_COUNT"] != "2" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 2", got["GIT_CONFIG_COUNT"])
	}
	if got["GIT_CONFIG_KEY_0"] != "credential.helper" || got["GIT_CONFIG_VALUE_0"] != "store" {
		t.Errorf("user gitconfig clobbered: %v", got)
	}
	if got["GIT_CONFIG_KEY_1"] != "safe.directory" || got["GIT_CONFIG_VALUE_1"] != dir {
		t.Errorf("safe.directory not appended at index 1: %v", got)
	}
}

// TestComposeEnv_SafeDirectoryRespectsUserDeclaration: if the caller
// already declared a safe.directory entry, we leave the env alone
// rather than appending a (possibly conflicting) one of our own.
func TestComposeEnv_SafeDirectoryRespectsUserDeclaration(t *testing.T) {
	dir := t.TempDir()
	user := map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "safe.directory",
		"GIT_CONFIG_VALUE_0": "/somewhere/else",
	}
	got := envToMap(composeEnv(sandboxSpec{Repo: dir, StepEnv: user}))
	if got["GIT_CONFIG_COUNT"] != "1" {
		t.Errorf("count bumped despite user safe.directory: %v", got)
	}
	if got["GIT_CONFIG_VALUE_0"] != "/somewhere/else" {
		t.Errorf("user safe.directory overwritten: %v", got)
	}
}

func TestComposeEnv_GOMAXPROCSFollowsBudget(t *testing.T) {
	t.Setenv(cpuBudgetEnv, "3")
	m := envToMap(composeEnv(sandboxSpec{HostEnv: []string{"PATH=/usr/bin"}}))
	if m["GOMAXPROCS"] != "3" {
		t.Fatalf("GOMAXPROCS = %q, want 3", m["GOMAXPROCS"])
	}
	m = envToMap(composeEnv(sandboxSpec{
		HostEnv: []string{"PATH=/usr/bin"},
		StepEnv: map[string]string{"GOMAXPROCS": "8"},
	}))
	if m["GOMAXPROCS"] != "8" {
		t.Fatalf("step GOMAXPROCS overridden: %q", m["GOMAXPROCS"])
	}
}

// TestComposeEnv_ModReadonlyInjected pins the guarantee that a task
// never rewrites the caller's go.mod/go.sum: outside a workspace a
// plain `go build` would add missing requirements straight into the
// bind-mounted checkout.
func TestComposeEnv_ModReadonlyInjected(t *testing.T) {
	got := envToMap(composeEnv(sandboxSpec{Cmdline: "go build ./..."}))
	if got["GOFLAGS"] != "-mod=readonly" {
		t.Errorf("GOFLAGS = %q, want %q", got["GOFLAGS"], "-mod=readonly")
	}
}

// TestComposeEnv_ModReadonlySkippedForManifestWriters confirms the
// injection is scoped: a step whose job is to update the manifests
// must not be forced into readonly, or it fails instead of working.
func TestComposeEnv_ModReadonlySkippedForManifestWriters(t *testing.T) {
	for _, cmd := range []string{
		"go mod tidy",
		"cd apps/core && go mod download",
		"go work sync",
		"go generate ./...",
		"go mod tidy && go build ./...",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := envToMap(composeEnv(sandboxSpec{Cmdline: cmd}))
			if strings.Contains(got["GOFLAGS"], "-mod=readonly") {
				t.Errorf("GOFLAGS = %q, want no -mod=readonly", got["GOFLAGS"])
			}
		})
	}
}

// TestComposeEnv_UserModFlagRespected ensures an explicit -mod= from
// the pipeline wins over the injected default.
func TestComposeEnv_UserModFlagRespected(t *testing.T) {
	got := envToMap(composeEnv(sandboxSpec{
		Cmdline: "go build ./...",
		StepEnv: map[string]string{"GOFLAGS": "-mod=mod"},
	}))
	if got["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want %q", got["GOFLAGS"], "-mod=mod")
	}
}
