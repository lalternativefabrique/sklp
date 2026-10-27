package spacemanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkalpaiDeployContract(t *testing.T) {
	for _, k := range []string{
		"DATABASE_URL", "POSTGRES_PASSWORD", "LLM_CREDENTIAL_ENCRYPTION_KEY",
		"S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BACKUP_BUCKET",
	} {
		t.Setenv(k, "placeholder")
	}
	path := filepath.Join("..", "..", "..", ".sklp", "skalpai", "deploy.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy contract not present: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	job, ok := cfg.Jobs["migrate"]
	if !ok {
		t.Fatal("no migrate job declared: core would start against an unmigrated schema")
	}
	if got := job.Env["DATABASE_URL"]; got == "" {
		t.Error("migrate job has no DATABASE_URL")
	}
	assertDependsOn(t, job.DependsOn, "timescaledb")
	assertDependsOn(t, cfg.Services["core"].DependsOn, "migrate")

	for _, svc := range cfg.Services {
		for k, v := range svc.Env {
			if k == "DATABASE_URL" && v != job.Env["DATABASE_URL"] {
				t.Errorf("service DSN %q diverges from the migrate job's %q", v, job.Env["DATABASE_URL"])
			}
		}
	}
}

func assertDependsOn(t *testing.T, deps []string, want string) {
	t.Helper()
	for _, d := range deps {
		if d == want {
			return
		}
	}
	t.Errorf("missing depends_on %q (got %v)", want, deps)
}

// The backup chain is only as good as its weakest link, and each link fails
// silently: a dump with nowhere to land, dumps that never leave the VM, or a
// restore nobody ever tried. Pin all three.
func TestSkalpaiBackupContract(t *testing.T) {
	for _, k := range []string{
		"DATABASE_URL", "POSTGRES_PASSWORD", "LLM_CREDENTIAL_ENCRYPTION_KEY",
		"S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BACKUP_BUCKET",
	} {
		t.Setenv(k, "placeholder")
	}
	path := filepath.Join("..", "..", "..", ".sklp", "skalpai", "deploy.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy contract not present: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	db, ok := cfg.Services["timescaledb"]
	if !ok {
		t.Fatal("no timescaledb service")
	}
	if db.Backup == nil {
		t.Fatal("timescaledb declares no backup: the database is unprotected")
	}
	// The dump has to land on a volume, or it dies with the container that
	// wrote it — before anything gets a chance to ship it.
	assertVolume(t, db.Volumes, "skalpai-dumps")

	ship, ok := cfg.Jobs["ship-dumps"]
	if !ok {
		t.Fatal("no ship-dumps job: dumps would never leave the VM")
	}
	if ship.Schedule == "" {
		t.Error("ship-dumps has no schedule, so it would run once and never again")
	}
	assertVolume(t, ship.Volumes, "skalpai-dumps")

	check, ok := cfg.Jobs["restore-check"]
	if !ok {
		t.Fatal("no restore-check job: the backups would never be verified")
	}
	if check.Schedule == "" {
		t.Error("restore-check has no schedule")
	}

	svcs, _, err := cfg.Resolve("production", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := map[string]bool{"timescaledb": false, "ship-dumps": true, "restore-check": true}
	got := map[string]bool{}
	for _, e := range scheduleEntries(svcs) {
		if _, err := ParseCron(e.expr); err != nil {
			t.Errorf("the supervisor would ignore %s's schedule %q: %v", e.service, e.expr, err)
		}
		got[e.service] = e.job
	}
	for name, isJob := range want {
		kind, ok := got[name]
		if !ok {
			t.Errorf("%s never reaches the scheduler", name)
			continue
		}
		if kind != isJob {
			t.Errorf("%s scheduled as job=%v, want job=%v", name, kind, isJob)
		}
	}
}

func assertVolume(t *testing.T, vols []string, name string) {
	t.Helper()
	for _, v := range vols {
		if strings.HasPrefix(v, name+":") {
			return
		}
	}
	t.Errorf("missing volume %q (got %v)", name, vols)
}

// WAL archiving is the difference between losing a day and losing a minute, and
// it is silently inert if any of its three parts is missing: the archiver, the
// sidecar that owns the base backups the WAL replays onto, and a drill proving
// the repo restores. The dump chain above is a separate guarantee with its own
// failure modes — a green dump says nothing about PITR.
func TestSkalpaiPITRContract(t *testing.T) {
	for _, k := range []string{
		"DATABASE_URL", "POSTGRES_PASSWORD", "LLM_CREDENTIAL_ENCRYPTION_KEY",
		"S3_ACCESS_KEY", "S3_SECRET_KEY", "S3_BACKUP_BUCKET",
	} {
		t.Setenv(k, "placeholder")
	}
	path := filepath.Join("..", "..", "..", ".sklp", "skalpai", "deploy.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy contract not present: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	db := cfg.Services["timescaledb"]
	// Plain postgres has no pgbackrest binary, so archive_command would fail on
	// every WAL segment while the cluster reports itself healthy.
	if !strings.Contains(db.Image, "pgbackrest") {
		t.Errorf("timescaledb image %q carries no pgbackrest: archive_command cannot run", db.Image)
	}
	if db.Env["PGBACKREST_STANZA"] == "" {
		t.Error("timescaledb has no PGBACKREST_STANZA")
	}
	assertVolume(t, db.Volumes, "skalpai-wal-spool")

	side, ok := cfg.Services["pgbackrest"]
	if !ok {
		t.Fatal("no pgbackrest sidecar: WAL would accumulate with no base backup to replay onto")
	}
	// The sidecar backs up the cluster by reading PGDATA directly. Without the
	// shared volume there is nothing to back up.
	assertVolume(t, side.Volumes, "skalpai-timescaledb-data")
	assertVolume(t, side.Volumes, "skalpai-wal-spool")
	if side.Env["PGBACKREST_STANZA"] != db.Env["PGBACKREST_STANZA"] {
		t.Errorf("stanza mismatch: sidecar %q vs postgres %q — they would address different repos",
			side.Env["PGBACKREST_STANZA"], db.Env["PGBACKREST_STANZA"])
	}

	if drill, ok := cfg.Jobs["pitr-check"]; !ok {
		t.Error("no pitr-check job: the WAL repo would never be proven restorable")
	} else if drill.Schedule == "" {
		t.Error("pitr-check has no schedule")
	}

	if nb, ok := cfg.Jobs["nats-backup"]; !ok {
		t.Error("no nats-backup job: JetStream state is on a volume and nothing copies it off")
	} else if nb.Schedule == "" {
		t.Error("nats-backup has no schedule")
	}

	drill, ok := cfg.Jobs["nats-restore-check"]
	if !ok {
		t.Fatal("no nats-restore-check job: the NATS backups would never be proven restorable")
	}
	if drill.Schedule == "" {
		t.Error("nats-restore-check has no schedule")
	}
	// `nats account restore` overwrites the streams it lands on. Pointed at the
	// live server, the drill meant to protect the data would destroy it.
	target := drill.Env["NATS_TARGET"]
	if target == "" {
		t.Fatal("nats-restore-check has no NATS_TARGET")
	}
	if strings.Contains(target, "nats://nats:") {
		t.Fatalf("nats-restore-check restores onto the live server (%s): a restore overwrites what it lands on", target)
	}
	if _, ok := cfg.Services["nats-restore-target"]; !ok {
		t.Error("no nats-restore-target service for the drill to restore into")
	}
}
