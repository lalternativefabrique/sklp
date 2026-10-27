package natsjs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/restore/objstore"
	"github.com/lalternativefabrique/sklp/restore/proc"
)

func natsCLI(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("nats")
	if err != nil {
		t.Skip("nats CLI not installed")
	}
	return p
}

type account struct {
	user    string
	streams map[string]int
}

// accountBackupArchive builds what the nats-backup image uploads: a live
// server is seeded, `nats account backup` runs per user, and the directory is
// archived under the same top-level name.
func accountBackupArchive(t *testing.T, accounts []account, dst string) {
	t.Helper()
	bin, cli := serverBin(t), natsCLI(t)
	port, _ := proc.FreePort()
	work := t.TempDir()
	conf := fmt.Sprintf("listen: 127.0.0.1:%d\njetstream { store_dir: %q }\n", port, filepath.Join(work, "store"))
	multi := len(accounts) > 1 || accounts[0].user != ""
	if multi {
		conf += "accounts {\n"
		for _, a := range accounts {
			conf += fmt.Sprintf("  %q: { jetstream: enabled, users: [{ user: %q, password: pw }] }\n", a.user, a.user)
		}
		conf += "}\n"
	}
	confPath := filepath.Join(work, "server.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := exec.Command(bin, "-c", confPath)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = srv.Process.Signal(syscall.SIGTERM)
		_ = srv.Wait()
	}()

	url := fmt.Sprintf("nats://127.0.0.1:%d", port)
	run := func(a account, args ...string) {
		t.Helper()
		full := append([]string{"--server", url}, args...)
		if a.user != "" {
			full = append(full, "--user", a.user, "--password", "pw")
		}
		var out []byte
		var err error
		for i := 0; i < 50; i++ {
			if out, err = exec.Command(cli, full...).CombinedOutput(); err == nil || !strings.Contains(string(out), "connection refused") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("nats %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	top := filepath.Join(work, "nats-backup-20261007T120000Z")
	for _, a := range accounts {
		for stream, n := range a.streams {
			subj := strings.ToLower(stream) + ".>"
			run(a, "stream", "add", stream, "--subjects", subj, "--storage", "file", "--defaults")
			for i := 0; i < n; i++ {
				run(a, "pub", strings.ToLower(stream)+".x", fmt.Sprintf(`{"n":%d}`, i))
			}
		}
		dest := top
		if multi {
			dest = filepath.Join(top, a.user)
		}
		run(a, "account", "backup", dest, "--force")
	}
	archive := t.TempDir()
	if err := os.Rename(top, filepath.Join(archive, filepath.Base(top))); err != nil {
		t.Fatal(err)
	}
	tarGz(t, archive, dst)
}

func counts(rep drill.Report) map[string]int64 {
	out := map[string]int64{}
	for _, c := range rep.Inventory.Collections {
		out[c.Group+"/"+c.Name] = c.Count
	}
	return out
}

func runAccountDrill(t *testing.T, bucket string) drill.Report {
	t.Helper()
	eng := New(Config{Store: objstore.Dir(bucket), Bucket: "app-backups", Prefix: "nats/", Scratch: t.TempDir(), ServerBin: serverBin(t), Ready: time.Minute})
	rep := drill.Run(context.Background(), eng, drill.Options{Space: "app-production", MaxRPO: time.Hour})
	var out strings.Builder
	rep.Render(&out)
	if rep.Status != drill.StatusOK {
		t.Fatalf("drill failed:\n%s", out.String())
	}
	if rep.Backup.Type != accountArchive {
		t.Fatalf("backup type = %s, want %s", rep.Backup.Type, accountArchive)
	}
	return rep
}

func TestDrillRestoresASingleAccountBackup(t *testing.T) {
	bucket := t.TempDir()
	accountBackupArchive(t, []account{{streams: map[string]int{"EVENTS": 25, "ORDERS": 3}}},
		filepath.Join(bucket, "nats", "nats-account-20261007T120000Z.tar.gz"))

	got := counts(runAccountDrill(t, bucket))
	if got["$G/EVENTS"] != 25 || got["$G/ORDERS"] != 3 || len(got) != 2 {
		t.Fatalf("restored = %v, want $G/EVENTS=25 and $G/ORDERS=3", got)
	}
}

func TestDrillRestoresOneDirectoryPerBackupUser(t *testing.T) {
	bucket := t.TempDir()
	accountBackupArchive(t, []account{
		{user: "backup-shop", streams: map[string]int{"PAYMENTS": 4}},
		{user: "backup-bakery", streams: map[string]int{"IDENTITY_EVENTS": 7}},
	}, filepath.Join(bucket, "nats", "nats-account-20261007T120000Z.tar.gz"))

	got := counts(runAccountDrill(t, bucket))
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got["backup-shop/PAYMENTS"] != 4 || got["backup-bakery/IDENTITY_EVENTS"] != 7 || len(got) != 2 {
		t.Fatalf("restored = %v (%v)", got, keys)
	}
}

func TestDrillPicksAnAccountBackupNewerThanAStoreArchive(t *testing.T) {
	bucket := t.TempDir()
	stale := filepath.Join(bucket, "nats", "nats-2026-09-29T0631.tar.gz")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(stale, past, past)
	accountBackupArchive(t, []account{{streams: map[string]int{"EVENTS": 2}}},
		filepath.Join(bucket, "nats", "nats-account-20261007T120000Z.tar.gz"))

	if got := counts(runAccountDrill(t, bucket)); got["$G/EVENTS"] != 2 {
		t.Fatalf("restored = %v", got)
	}
}
