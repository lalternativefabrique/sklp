package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCentralLogDir_RepoSlug(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/cache")
	cases := map[string]string{
		"/home/u/code/projects/acme":     "/cache/sklp/logs/acme",
		"/home/u/code/projects/skalpai/": "/cache/sklp/logs/skalpai",
		"":                               "/cache/sklp/logs/unknown",
	}
	for in, want := range cases {
		if got := CentralLogDir(in); got != want {
			t.Errorf("CentralLogDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCentralLogRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/cache")
	if got, want := CentralLogRoot(), "/cache/sklp/logs"; got != want {
		t.Errorf("CentralLogRoot() = %q, want %q", got, want)
	}
}

// LatestCentralCILog must find the newest ci-*.log across ALL repos, not just
// the cwd's — the whole point of centralisation.
func TestLatestCentralCILog_AcrossRepos(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	root := filepath.Join(cache, "sklp", "logs")

	write := func(repo, name string) string {
		dir := filepath.Join(root, repo)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	write("acme", "ci-20260601-100000.log")
	write("acme", "not-a-ci.log")                        // ignored: wrong prefix
	newest := write("skalpai", "ci-20260612-090000.log") // newest globally
	write("skalpai", "ci-20260610-080000.log")

	if got := LatestCentralCILog(); got != newest {
		t.Errorf("LatestCentralCILog() = %q, want %q", got, newest)
	}
}

func TestLatestCentralCILog_Empty(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if got := LatestCentralCILog(); got != "" {
		t.Errorf("LatestCentralCILog() = %q, want empty", got)
	}
}

// LatestCILogForRepo must return THIS repo's newest log, never another repo's
// newer one — the fix for the cockpit tailing a stray /tmp/Test... log.
func TestLatestCILogForRepo_ScopedToRepo(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	root := filepath.Join(cache, "sklp", "logs")
	write := func(slug, name string) string {
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A newer log in ANOTHER repo must NOT be returned for acme.
	write("skalpai", "ci-20260612-090000.log")
	want := write("acme", "ci-20260601-100000.log")

	if got := LatestCILogForRepo("/home/u/code/projects/acme"); got != want {
		t.Errorf("LatestCILogForRepo(acme) = %q, want %q", got, want)
	}
	if got := LatestCILogForRepo("/home/u/code/projects/never-ran"); got != "" {
		t.Errorf("LatestCILogForRepo(never-ran) = %q, want empty", got)
	}
}

func TestTaskFilenameSlug(t *testing.T) {
	cases := map[string]string{
		"ci-sklp":         "ci-sklp",
		"secops":          "secops",
		"Test Core DB":    "test-core-db",
		"  weird__name  ": "weird-name",
		"":                "",
	}
	for in, want := range cases {
		if got := TaskFilenameSlug(in); got != want {
			t.Errorf("TaskFilenameSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCILogName(t *testing.T) {
	if got := ciLogName("ci-sklp", "20260612-090429"); got != "ci-ci-sklp-20260612-090429.log" {
		t.Errorf("ciLogName(task) = %q", got)
	}
	if got := ciLogName("", "20260612-090429"); got != "ci-20260612-090429.log" {
		t.Errorf("ciLogName(no task) = %q", got)
	}
}

func TestLogTimestamp(t *testing.T) {
	cases := map[string]string{
		"ci-sklp-20260612-090429.log":         "20260612-090429",
		"ci-20260612-090429.log":              "20260612-090429",
		"ci-test-core-db-20260612-090429.log": "20260612-090429",
	}
	for in, want := range cases {
		if got := logTimestamp(in); got != want {
			t.Errorf("logTimestamp(%q) = %q, want %q", in, got, want)
		}
	}
}

// LatestCILogForTask must return THIS task's newest log, never another task's
// newer one in the same repo — the fix for every CI space tailing the repo's
// most recent log.
func TestLatestCILogForTask_ScopedToTask(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	dir := filepath.Join(cache, "sklp", "logs", "skalpai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// secops has a NEWER timestamp than ci-sklp; asking for ci-sklp must still
	// return ci-sklp's log, not secops'.
	want := write("ci-ci-sklp-20260612-090000.log")
	write("ci-secops-20260612-100000.log")
	write("ci-ci-skt-20260612-095959.log")

	repo := "/home/u/code/projects/skalpai"
	if got := LatestCILogForTask(repo, "ci-sklp"); got != want {
		t.Errorf("LatestCILogForTask(ci-sklp) = %q, want %q", got, want)
	}
	if got := LatestCILogForTask(repo, "never-ran"); got != "" {
		t.Errorf("LatestCILogForTask(never-ran) = %q, want empty", got)
	}
}

// Newest-by-timestamp must ignore the task segment's length (lexical full-name
// compare would let a longer task name win regardless of time).
func TestLatestCILogForTask_NewestByTimestampNotName(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	dir := filepath.Join(cache, "sklp", "logs", "skalpai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	older := filepath.Join(dir, "ci-ci-sklp-20260612-080000.log")
	newer := filepath.Join(dir, "ci-ci-sklp-20260612-090000.log")
	for _, p := range []string{older, newer} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := LatestCILogForTask("/x/skalpai", "ci-sklp"); got != newer {
		t.Errorf("got %q, want newest %q", got, newer)
	}
}

// A dev run-log must resolve by stack and never collide with a CI log that
// carries the same task name: the cockpit picks the panel content from these
// two resolvers alone.
func TestLatestDevLogForStack(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	repo := "/home/u/code/projects/acme"
	dir := CentralLogDir(repo)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"dev-reranker-20260805-090000.log",
		"dev-reranker-20260805-100000.log",
		"dev-infra-20260805-110000.log",
		"ci-reranker-20260805-120000.log",
	} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got, want := LatestDevLogForStack(repo, "reranker"), filepath.Join(dir, "dev-reranker-20260805-100000.log"); got != want {
		t.Errorf("newest reranker dev log = %q, want %q", got, want)
	}
	// An empty stack takes the newest dev log of any stack — never the ci- one,
	// even though it is the newest file in the directory.
	if got, want := LatestDevLogForStack(repo, ""), filepath.Join(dir, "dev-infra-20260805-110000.log"); got != want {
		t.Errorf("repo-wide newest dev log = %q, want %q", got, want)
	}
	if got := LatestDevLogForStack(repo, "absent"); got != "" {
		t.Errorf("unknown stack should have no log, got %q", got)
	}
}
