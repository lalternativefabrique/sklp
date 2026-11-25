package importer

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/devstack"
	"github.com/lalternativefabrique/sklp/internal/repofacts"
)

var update = flag.Bool("update", false, "rewrite the golden dev.yaml files")

func importFixture(t *testing.T, name string) *ComposeImport {
	t.Helper()
	root := filepath.Join("testdata", name)
	runnables, err := repofacts.ScanRunnable(root)
	if err != nil {
		t.Fatalf("scan runnables: %v", err)
	}
	res, err := ImportCompose(ComposeOptions{Root: root, Stack: "app", Runnables: runnables})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res == nil {
		t.Fatal("no compose file found")
	}
	return res
}

func TestImportedDevStackMatchesGoldenAndLoads(t *testing.T) {
	for _, name := range []string{"fullstack", "unsupported", "infra-only"} {
		t.Run(name, func(t *testing.T) {
			res := importFixture(t, name)
			golden := filepath.Join("testdata", name+".dev.yaml")
			if *update {
				if err := os.WriteFile(golden, res.YAML, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if string(want) != string(res.YAML) {
				t.Errorf("dev.yaml drifted from %s (run with -update and review the diff):\n%s", golden, res.YAML)
			}

			out := filepath.Join(t.TempDir(), "dev.yaml")
			if err := os.WriteFile(out, res.YAML, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := devstack.LoadOpts(out, false); err != nil {
				t.Fatalf("sklp dev rejects the imported file: %v", err)
			}
		})
	}
}

func notesFor(notes []Note, service, field string) []Note {
	var out []Note
	for _, n := range notes {
		if n.Service == service && (field == "" || n.Field == field) {
			out = append(out, n)
		}
	}
	return out
}

func requireNote(t *testing.T, notes []Note, sev Severity, service, field, fragment string) {
	t.Helper()
	for _, n := range notesFor(notes, service, field) {
		if n.Severity == sev && strings.Contains(n.Message, fragment) {
			return
		}
	}
	t.Errorf("missing %v note on %s.%s containing %q; got:\n%s", sev, service, field, fragment, dumpNotes(notes))
}

func dumpNotes(notes []Note) string {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString("  " + n.String() + "\n")
	}
	return b.String()
}

func TestFullstackTranslation(t *testing.T) {
	res := importFixture(t, "fullstack")
	y := string(res.YAML)

	for _, want := range []string{
		"image: postgres:16-alpine",
		"pgdata:/var/lib/postgresql/data",
		"POSTGRES_PASSWORD: secret",
		"TZ: Europe/Paris",
		"command: [redis-server, --appendonly, \"yes\"]",
		"REDIS_ARGS: --save 60 1",
		"run: go run ./cmd/api",
		"workdir: api",
		"DATABASE_URL: postgres://app:secret@postgres:5432/app",
		"LOG_LEVEL: debug",
		"http: /healthz",
		"log: database system is ready to accept connections",
		"oneshot: true",
		"optional: true",
		"run: npm run dev",
		"- .env.api",
	} {
		if !strings.Contains(y, want) {
			t.Errorf("dev.yaml lacks %q:\n%s", want, y)
		}
	}
	if strings.Contains(y, "${") {
		t.Errorf("an interpolation survived although every variable has a value:\n%s", y)
	}

	notes := res.Notes
	requireNote(t, notes, Info, "postgres", "ports", "sklp dev forward app postgres:5432 5433")
	requireNote(t, notes, Info, "postgres", "healthcheck", "pg_isready -U app")
	requireNote(t, notes, Info, "postgres", "restart", "does not restart")
	requireNote(t, notes, Manual, "postgres", "volumes", "COPY db/init.sql /docker-entrypoint-initdb.d/init.sql")
	requireNote(t, notes, Manual, "migrate", "volumes", "only mounts named volumes")
	if strings.Contains(yamlBody(res.YAML), "./db/") {
		t.Errorf("host binds must not be emitted, sklp refuses them:\n%s", yamlBody(res.YAML))
	}
	requireNote(t, notes, Info, "migrate", "oneshot", "service_completed_successfully")
	requireNote(t, notes, Manual, "api", "build", "GO_VERSION")
	requireNote(t, notes, Manual, "api", "build", "libvips-dev")
	requireNote(t, notes, Manual, "api", "environment", "set API_KEY in .env")
	requireNote(t, notes, Info, "api", "volumes", "./api:/app dropped")
	requireNote(t, notes, Info, "web", "volumes", "node_modules")
	requireNote(t, notes, Manual, "web", "build", "apk (Alpine) packages git")
	requireNote(t, notes, Info, "web", "ports", "sklp dev forward app web:5173 3000")
	requireNote(t, notes, Manual, "mailpit", "profiles", "never starts optional services")
	requireNote(t, notes, Info, "mailpit", "ports", "1025")
	requireNote(t, notes, Info, "", "volumes", "pgdata")
}

func TestUnsupportedFieldsBecomeManualActions(t *testing.T) {
	res := importFixture(t, "unsupported")
	notes := res.Notes

	for _, field := range []string{"entrypoint", "privileged", "cap_add", "network_mode", "user", "secrets", "tmpfs", "frobnicate", "extra_hosts"} {
		found := false
		for _, n := range notesFor(notes, "proxy", field) {
			found = found || n.Severity == Manual
		}
		if !found {
			t.Errorf("proxy.%s should be a manual action; got:\n%s", field, dumpNotes(notes))
		}
	}
	requireNote(t, notes, Manual, "proxy", "volumes", "no Docker daemon")
	requireNote(t, notes, Manual, "proxy", "volumes", "~/.config/traefik:/etc/traefik is a host bind mount")
	requireNote(t, notes, Manual, "proxy", "volumes", "certs is external")
	requireNote(t, notes, Manual, "worker", "run", "image: nginx:1.27")
	requireNote(t, notes, Manual, "worker", "run", "nginx.conf")
	requireNote(t, notes, Manual, "worker", "networks", "jobs")
	requireNote(t, notes, Manual, "worker", "depends_on", "ghost")
	requireNote(t, notes, Manual, "worker", "ports", "9000-9002")
	requireNote(t, notes, Manual, "worker", "ports", "53/udp")
	requireNote(t, notes, Manual, "", "include", "other.yaml")
	requireNote(t, notes, Manual, "", "secrets", "top-level secrets")

	if !strings.Contains(string(res.YAML), "allowHost: true") {
		t.Errorf("host-gateway should set allowHost:\n%s", res.YAML)
	}
	body := yamlBody(res.YAML)
	if strings.Contains(body, "docker.sock") || strings.Contains(body, "~/.config") {
		t.Errorf("unportable mounts must not be emitted:\n%s", body)
	}
	if got := todoComments(res.YAML); got != ManualCount(notes) {
		t.Errorf("file carries %d TODO(import) lines, report has %d manual actions", got, ManualCount(notes))
	}
}

func TestInfraOnlyComposeKeepsDetectedApps(t *testing.T) {
	res := importFixture(t, "infra-only")
	y := string(res.YAML)
	for _, want := range []string{"image: postgis/postgis:16-3.4", "command: [-js, -sd, /data]", "go run ./cmd/server"} {
		if !strings.Contains(y, want) {
			t.Errorf("dev.yaml lacks %q:\n%s", want, y)
		}
	}
	requireNote(t, res.Notes, Manual, "api", "", "compose services: db, nats")
}

func TestNoComposeFileImportsNothing(t *testing.T) {
	res, err := ImportCompose(ComposeOptions{Root: t.TempDir()})
	if err != nil || res != nil {
		t.Fatalf("want nil, nil; got %v, %v", res, err)
	}
}

func TestExpandFollowsComposeInterpolation(t *testing.T) {
	vs := &varSource{
		template: map[string]string{"SET": "v", "EMPTY": ""},
		local:    map[string]bool{"LOCAL": true},
	}
	cases := []struct {
		in, want   string
		unresolved bool
	}{
		{"plain", "plain", false},
		{"$$HOME", "$HOME", false},
		{"${SET}", "v", false},
		{"$SET/x", "v/x", false},
		{"${UNSET:-d}", "d", false},
		{"${EMPTY:-d}", "d", false},
		{"${EMPTY-d}", "", false},
		{"${UNSET-d}", "d", false},
		{"${SET:+alt}", "alt", false},
		{"${UNSET:+alt}", "", false},
		{"${UNSET:-${SET}}", "v", false},
		{"${UNSET}", "${UNSET}", true},
		{"${UNSET:?boom}", "${UNSET:?boom}", true},
		{"a-${LOCAL:-dflt}-b", "a-dflt-b", false},
	}
	for _, c := range cases {
		e := vs.expand(c.in)
		if e.value != c.want || e.resolved() == c.unresolved {
			t.Errorf("expand(%q) = %q (unresolved %v), want %q (unresolved %v)", c.in, e.value, e.unresolved, c.want, c.unresolved)
		}
	}
	if e := vs.expand("${LOCAL:-x}"); len(e.local) != 1 {
		t.Errorf("a variable set in .env must be reported, got %+v", e)
	}
}

func TestShellSplitAndJoinRoundTrip(t *testing.T) {
	words, err := shellSplit(`sh -c "echo 'hi there' && exit 0" plain\ space`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-c", "echo 'hi there' && exit 0", "plain space"}
	if strings.Join(words, "|") != strings.Join(want, "|") {
		t.Fatalf("split = %q, want %q", words, want)
	}
	back, err := shellSplit(shellJoin(words))
	if err != nil || strings.Join(back, "|") != strings.Join(want, "|") {
		t.Fatalf("join did not round-trip: %q (%v)", back, err)
	}
	if _, err := shellSplit(`echo "open`); err == nil {
		t.Fatal("unterminated quote must fail")
	}
}

func yamlBody(y []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(y), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func todoComments(y []byte) int {
	n := 0
	for _, line := range strings.Split(string(y), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# TODO(import):") {
			n++
		}
	}
	return n
}

func TestPostgres18MountMovesToTheNewDataRoot(t *testing.T) {
	cases := map[string]bool{
		"postgres:latest":                   true,
		"postgres:alpine":                   true,
		"postgres":                          true,
		"postgres:18":                       true,
		"postgres:18.1-alpine":              true,
		"postgres:16-alpine":                false,
		"postgis/postgis:16-3.4":            false,
		"timescale/timescaledb:latest-pg16": false,
		"mariadb:10.6":                      false,
	}
	for image, want := range cases {
		if got := postgres18OrUnpinned(image); got != want {
			t.Errorf("postgres18OrUnpinned(%q) = %v, want %v", image, got, want)
		}
	}
}
