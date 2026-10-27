package postgres

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
)

// fakePgbackrest answers `info` from a fixture and "restores" by copying a
// prepared cluster into pg1-path, so everything after the repository — start,
// counting, pg_amcheck, delta reuse — runs against a real PostgreSQL.
const fakePgbackrest = `#!/bin/sh
echo "$@" >> "$FAKE_CALLS"
for a in "$@"; do
  case "$a" in
    --config=*) conf="${a#--config=}" ;;
    info) cat "$FAKE_INFO"; exit 0 ;;
    restore)
      dest=$(sed -n 's/^pg1-path=//p' "$conf")
      [ -n "$PGBACKREST_REPO1_S3_KEY" ] || { echo "no key in env" >&2; exit 1; }
      rm -rf "$dest"/* && cp -a "$FAKE_SOURCE"/. "$dest"/ && rm -f "$dest/postmaster.pid"
      exit 0 ;;
  esac
done
exit 1
`

func requirePostgres(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"initdb", "pg_ctl", "psql", "pg_amcheck"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

func localMajor(t *testing.T) int {
	out, err := exec.Command("pg_ctl", "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return majorOf(strings.TrimPrefix(strings.TrimSpace(string(out)), "pg_ctl (PostgreSQL) "))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// sourceCluster builds a stopped cluster holding two databases with known
// row counts, standing in for what the backup contains.
func sourceCluster(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "pgdata")
	sock := filepath.Join(dir, "sock")
	if err := os.Mkdir(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	run(t, "initdb", "-D", data, "-U", "app", "--auth=trust", "--no-sync")
	run(t, "pg_ctl", "-D", data, "-w", "-o", fmt.Sprintf("-p 5499 -c listen_addresses='' -c unix_socket_directories='%s'", sock), "-l", filepath.Join(dir, "log"), "start")
	defer run(t, "pg_ctl", "-D", data, "-m", "fast", "-w", "stop")
	psql := func(db, sql string) {
		run(t, "psql", "-h", sock, "-p", "5499", "-U", "app", "-d", db, "-v", "ON_ERROR_STOP=1", "-qc", sql)
	}
	psql("postgres", "create database app")
	psql("postgres", "create database audit")
	psql("app", `create schema billing;
create table public.users (id serial primary key, email text unique);
insert into public.users (email) select 'u' || g || '@x' from generate_series(1, 1234) g;
create table billing.invoices (id int, at date) partition by range (at);
create table billing.invoices_2026 partition of billing.invoices for values from ('2026-01-01') to ('2027-01-01');
insert into billing.invoices select g, '2026-06-01' from generate_series(1, 77) g;
create table public.empty ();`)
	psql("audit", "create table log (line text); insert into log select 'l' from generate_series(1, 5)")
	return data
}

type harness struct {
	eng   *Engine
	calls string
}

func newHarness(t *testing.T, source string, major int) harness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pgbackrest")
	if err := os.WriteFile(bin, []byte(fakePgbackrest), 0o755); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(dir, "info.json")
	stop := time.Date(2026, 9, 26, 3, 0, 8, 0, time.UTC).Unix()
	fixture := fmt.Sprintf(`[{"name":"app","status":{"code":0,"message":"ok"},
"db":[{"id":1,"version":"%d"},{"id":2,"version":"%d"}],
"backup":[
 {"label":"20260924-085525F","type":"full","error":false,"timestamp":{"stop":%d},"info":{"size":1000},"database":{"id":2}},
 {"label":"20260924-085525F_20260926-030002D","type":"diff","error":false,"timestamp":{"stop":%d},"info":{"size":33554432},"database":{"id":2}},
 {"label":"20260926-040000D","type":"diff","error":true,"timestamp":{"stop":%d},"info":{"size":1},"database":{"id":2}}]}]`,
		major-1, major, stop-86400, stop, stop+3600)
	if err := os.WriteFile(info, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	t.Setenv("FAKE_INFO", info)
	t.Setenv("FAKE_SOURCE", source)
	t.Setenv("FAKE_CALLS", calls)
	return harness{
		eng: New(Config{
			Stanza:   "app",
			Repo:     Repo{Endpoint: "s3.example", Region: "gra", Bucket: "app-backups", AccessKey: "AK", SecretKey: "SK"},
			User:     "app",
			Scratch:  t.TempDir(),
			Replay:   time.Minute,
			KeepData: true,
			Bin:      bin,
		}),
		calls: calls,
	}
}

func TestDrillCountsEveryRowOfTheNewestValidBackup(t *testing.T) {
	requirePostgres(t)
	h := newHarness(t, sourceCluster(t), localMajor(t))
	rep := drill.Run(context.Background(), h.eng, drill.Options{Space: "app-production"})

	var out strings.Builder
	rep.Render(&out)
	if rep.Status != drill.StatusOK {
		t.Fatalf("drill failed:\n%s", out.String())
	}
	if rep.Backup.Label != "20260924-085525F_20260926-030002D" {
		t.Fatalf("restored %s, want the newest backup that did not error", rep.Backup.Label)
	}
	want := map[string]int64{
		"app/public.users":     1234,
		"app/billing.invoices": 77,
		"app/public.empty":     0,
		"audit/public.log":     5,
	}
	got := map[string]int64{}
	for _, c := range rep.Inventory.Collections {
		got[c.Group+"/"+c.Name] = c.Count
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s = %d rows, want %d\n%s", k, got[k], n, out.String())
		}
	}
	if _, ok := got["app/billing.invoices_2026"]; ok {
		t.Error("a partition was counted on its own, doubling its parent's rows")
	}
	if !rep.Inventory.RecoveryPoint.Equal(time.Date(2026, 9, 26, 3, 0, 8, 0, time.UTC)) {
		t.Errorf("recovery point = %s, want the backup stop time on an idle cluster", rep.Inventory.RecoveryPoint)
	}
	for _, c := range rep.Checks {
		if c.Name == "integrity" && !strings.Contains(c.Detail, "pg_amcheck") {
			t.Errorf("integrity check = %+v", c)
		}
	}
	conf, _ := os.ReadFile(h.eng.conf())
	if strings.Contains(string(conf), "SK") || strings.Contains(string(conf), "AK") {
		t.Error("the S3 keys were written to the scratch volume")
	}
}

func TestSecondDrillRestoresInDelta(t *testing.T) {
	requirePostgres(t)
	h := newHarness(t, sourceCluster(t), localMajor(t))
	for i := 0; i < 2; i++ {
		if rep := drill.Run(context.Background(), h.eng, drill.Options{}); rep.Status != drill.StatusOK {
			t.Fatalf("run %d: %v", i, rep.Failures())
		}
	}
	calls, _ := os.ReadFile(h.calls)
	var restores []string
	for _, l := range strings.Split(string(calls), "\n") {
		if strings.HasSuffix(l, " restore") {
			restores = append(restores, l)
		}
	}
	if len(restores) != 2 || strings.Contains(restores[0], "--delta") || !strings.Contains(restores[1], "--delta") {
		t.Fatalf("restores = %q, want a full restore then a delta one", restores)
	}
}

func TestDrillRefusesABackupFromAnotherMajor(t *testing.T) {
	requirePostgres(t)
	h := newHarness(t, t.TempDir(), localMajor(t)+1)
	rep := drill.Run(context.Background(), h.eng, drill.Options{})
	if rep.Status != drill.StatusFailed || !strings.Contains(rep.Summary(), "use the image matching production") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestNewestBackupSkipsErroredSetsAndUnknownStanzas(t *testing.T) {
	raw := []byte(`[{"name":"a","status":{"code":0},"db":[{"id":1,"version":"17"}],"backup":[
{"label":"F","type":"full","error":false,"timestamp":{"stop":100},"info":{"size":5},"database":{"id":1}},
{"label":"D","type":"diff","error":true,"timestamp":{"stop":200},"info":{"size":1},"database":{"id":1}}]}]`)
	b, err := newestBackup(raw, "a")
	if err != nil || b.Label != "F" || b.Major != 17 || b.Bytes != 5 {
		t.Fatalf("got %+v, %v", b, err)
	}
	if _, err := newestBackup(raw, "other"); err == nil || !strings.Contains(err.Error(), "not in the repository") {
		t.Fatalf("unknown stanza: %v", err)
	}
	missing := []byte(`[{"name":"a","status":{"code":1,"message":"missing stanza path"},"db":[],"backup":[]}]`)
	if _, err := newestBackup(missing, "a"); err == nil || !strings.Contains(err.Error(), "missing stanza path") {
		t.Fatalf("missing stanza: %v", err)
	}
}
