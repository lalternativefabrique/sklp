package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/restore/proc"
)

type Repo struct {
	Endpoint  string
	Region    string
	Bucket    string
	Path      string
	AccessKey string
	SecretKey string
}

type Config struct {
	Stanza     string
	Repo       Repo
	User       string
	Scratch    string
	ProcessMax int
	Replay     time.Duration
	KeepData   bool
	Identity   *proc.Identity
	Bin        string
	BinDir     string
}

type Engine struct {
	cfg    Config
	backup located
}

func New(cfg Config) *Engine {
	if cfg.Repo.Path == "" {
		cfg.Repo.Path = "/pgbackrest"
	}
	if cfg.User == "" {
		cfg.User = "postgres"
	}
	if cfg.ProcessMax <= 0 {
		cfg.ProcessMax = max(1, runtime.NumCPU())
	}
	if cfg.Replay == 0 {
		cfg.Replay = time.Hour
	}
	if cfg.Bin == "" {
		cfg.Bin = "pgbackrest"
	}
	return &Engine{cfg: cfg}
}

func (e *Engine) Kind() string   { return "postgres" }
func (e *Engine) Target() string { return e.cfg.Stanza }

func (e *Engine) work() string   { return filepath.Join(e.cfg.Scratch, "postgres") }
func (e *Engine) pgdata() string { return filepath.Join(e.work(), "pgdata") }
func (e *Engine) conf() string   { return filepath.Join(e.work(), "pgbackrest.conf") }

func (e *Engine) tool(name string) string {
	if e.cfg.BinDir != "" {
		return filepath.Join(e.cfg.BinDir, name)
	}
	return name
}

// prepare hands the scratch volume to the postgres user — the runtime creates
// it root-owned and 0700, which pgbackrest and postgres cannot even traverse —
// and writes a config holding no secret: the S3 keys reach pgbackrest through
// its environment only, so nothing sensitive is ever written to the volume.
func (e *Engine) prepare() error {
	if err := os.MkdirAll(e.cfg.Scratch, 0o700); err != nil {
		return err
	}
	if err := e.cfg.Identity.Own(e.cfg.Scratch); err != nil {
		return err
	}
	for _, d := range []string{e.work(), filepath.Join(e.work(), "log"), filepath.Join(e.work(), "lock")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := e.cfg.Identity.Own(d); err != nil {
			return err
		}
	}
	conf := fmt.Sprintf(`[global]
repo1-type=s3
repo1-s3-endpoint=%s
repo1-s3-region=%s
repo1-s3-bucket=%s
repo1-s3-uri-style=path
repo1-path=%s
log-path=%s
lock-path=%s
log-level-console=warn
log-level-file=off

[%s]
pg1-path=%s
pg1-port=%d
pg1-socket-path=%s
`, e.cfg.Repo.Endpoint, e.cfg.Repo.Region, e.cfg.Repo.Bucket, e.cfg.Repo.Path,
		filepath.Join(e.work(), "log"), filepath.Join(e.work(), "lock"),
		e.cfg.Stanza, e.pgdata(), port, e.work())
	if err := os.WriteFile(e.conf(), []byte(conf), 0o600); err != nil {
		return err
	}
	return e.cfg.Identity.Own(e.conf())
}

func (e *Engine) env() []string {
	return []string{
		"PGBACKREST_REPO1_S3_KEY=" + e.cfg.Repo.AccessKey,
		"PGBACKREST_REPO1_S3_KEY_SECRET=" + e.cfg.Repo.SecretKey,
	}
}

func (e *Engine) pgbackrest(ctx context.Context, args ...string) ([]byte, error) {
	return proc.Run(ctx, e.cfg.Identity, e.env(), e.cfg.Bin, append([]string{"--config=" + e.conf(), "--stanza=" + e.cfg.Stanza}, args...)...)
}

func (e *Engine) Locate(ctx context.Context) (drill.Backup, error) {
	if err := e.prepare(); err != nil {
		return drill.Backup{}, err
	}
	raw, err := e.pgbackrest(ctx, "--output=json", "info")
	if err != nil {
		return drill.Backup{}, err
	}
	b, err := newestBackup(raw, e.cfg.Stanza)
	if err != nil {
		return drill.Backup{}, err
	}
	local, err := e.localMajor(ctx)
	if err != nil {
		return b.Backup, err
	}
	if b.Major != 0 && b.Major != local {
		return b.Backup, fmt.Errorf("backup %s is PostgreSQL %d but this drill runs PostgreSQL %d: use the image matching production", b.Label, b.Major, local)
	}
	e.backup = b
	return b.Backup, nil
}

func (e *Engine) localMajor(ctx context.Context) (int, error) {
	out, err := proc.Run(ctx, nil, nil, e.tool("pg_ctl"), "--version")
	if err != nil {
		return 0, err
	}
	return majorOf(strings.TrimPrefix(strings.TrimSpace(string(out)), "pg_ctl (PostgreSQL) ")), nil
}

// Restore reuses the previous drill's data directory when there is one:
// --delta rewrites only the files that differ from the backup, which turns a
// nightly full restore into a transfer of what changed since yesterday.
func (e *Engine) Restore(ctx context.Context, b drill.Backup) error {
	_ = os.Remove(filepath.Join(e.pgdata(), "postmaster.pid"))
	args := []string{"--set=" + b.Label, "--type=default", fmt.Sprintf("--process-max=%d", e.cfg.ProcessMax)}
	if _, err := os.Stat(filepath.Join(e.pgdata(), "PG_VERSION")); err == nil {
		args = append(args, "--delta")
	} else {
		if err := os.RemoveAll(e.pgdata()); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(e.pgdata(), 0o700); err != nil {
		return err
	}
	if err := e.cfg.Identity.Own(e.pgdata()); err != nil {
		return err
	}
	_, err := e.pgbackrest(ctx, append(args, "restore")...)
	return err
}

const port = 5433

func (e *Engine) Start(ctx context.Context) (drill.Instance, error) {
	hba := filepath.Join(e.work(), "pg_hba.conf")
	if err := os.WriteFile(hba, []byte("local all all trust\n"), 0o600); err != nil {
		return nil, err
	}
	if err := e.cfg.Identity.Own(hba); err != nil {
		return nil, err
	}
	logPath := filepath.Join(e.work(), "postgres.log")
	opts := strings.Join([]string{
		fmt.Sprintf("-p %d", port),
		"-c listen_addresses=''",
		"-c unix_socket_directories='" + e.work() + "'",
		"-c hba_file='" + hba + "'",
		"-c archive_mode=off",
		"-c ssl=off",
		"-c shared_buffers=128MB",
		"-c logging_collector=off",
	}, " ")
	inst := &instance{e: e, log: logPath}
	if _, err := proc.Run(ctx, e.cfg.Identity, e.env(), e.tool("pg_ctl"), "-D", e.pgdata(), "-l", logPath, "-w",
		"-t", fmt.Sprint(int(e.cfg.Replay.Seconds())), "-o", opts, "start"); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, tailFile(logPath, 15))
	}
	if err := inst.waitPromoted(ctx); err != nil {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	return inst, nil
}

type instance struct {
	e   *Engine
	log string
}

// psql runs one statement and returns its rows. psql ships in every Postgres
// image the drill runs in, so the node needs no driver of its own.
func (i *instance) psql(ctx context.Context, db, sql string) ([][]string, error) {
	out, err := proc.Run(ctx, i.e.cfg.Identity,
		[]string{"PGOPTIONS=-c statement_timeout=600000", "PGAPPNAME=restore-drill"},
		i.e.tool("psql"), "-X", "-A", "-t", "-F", fieldSep, "-R", recordSep, "-v", "ON_ERROR_STOP=1",
		"-h", i.e.work(), "-p", fmt.Sprint(port), "-U", i.e.cfg.User, "-d", db, "-c", sql)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, rec := range strings.Split(strings.TrimRight(string(out), "\n"+recordSep), recordSep) {
		if rec = strings.TrimPrefix(rec, "\n"); rec != "" {
			rows = append(rows, strings.Split(rec, fieldSep))
		}
	}
	return rows, nil
}

const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// waitPromoted waits for WAL replay to reach the end of the archive: until the
// cluster leaves recovery, what it serves is not yet what the backup holds.
func (i *instance) waitPromoted(ctx context.Context) error {
	deadline := time.Now().Add(i.e.cfg.Replay)
	var last error
	for time.Now().Before(deadline) {
		rows, err := i.psql(ctx, "postgres", "select pg_is_in_recovery()")
		if err == nil && len(rows) == 1 && rows[0][0] == "f" {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("still replaying WAL after %s (%v)\n%s", i.e.cfg.Replay, last, tailFile(i.log, 15))
}

func (i *instance) Inspect(ctx context.Context) (drill.Inventory, error) {
	var inv drill.Inventory
	rows, err := i.psql(ctx, "postgres",
		"select coalesce(extract(epoch from pg_last_xact_replay_timestamp())::text, ''), pg_current_wal_lsn()::text")
	if err != nil {
		return inv, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return inv, fmt.Errorf("unexpected recovery row %q", rows)
	}
	inv.Position = rows[0][1]
	inv.RecoveryPoint = i.e.backup.TakenAt
	if replayed, ok := parseEpoch(rows[0][0]); ok && replayed.After(inv.RecoveryPoint) {
		inv.RecoveryPoint = replayed
	}

	dbs, err := i.psql(ctx, "postgres", "select datname, pg_database_size(oid) from pg_database where datallowconn and not datistemplate order by datname")
	if err != nil {
		return inv, err
	}
	for _, db := range dbs {
		size, _ := strconv.ParseInt(db[1], 10, 64)
		inv.Bytes += size
		cols, err := i.tables(ctx, db[0])
		if err != nil {
			return inv, fmt.Errorf("database %s: %w", db[0], err)
		}
		inv.Collections = append(inv.Collections, cols...)
	}
	return inv, nil
}

// tables counts every row exactly, in one statement per database. Partitions
// and TimescaleDB chunks are left out: their parent's count includes them.
func (i *instance) tables(ctx context.Context, db string) ([]drill.Collection, error) {
	rows, err := i.psql(ctx, db, `
select n.nspname, c.relname, pg_total_relation_size(c.oid)
from pg_class c join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p') and not c.relispartition
  and n.nspname not in ('pg_catalog', 'information_schema')
  and n.nspname not like 'pg\_toast%' and n.nspname not like '\_timescaledb%'
order by 1, 2`)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	counts := make([]string, len(rows))
	for idx, r := range rows {
		counts[idx] = fmt.Sprintf("select %d, count(*) from %s.%s", idx, quoteIdent(r[0]), quoteIdent(r[1]))
	}
	counted, err := i.psql(ctx, db, strings.Join(counts, " union all "))
	if err != nil {
		return nil, err
	}
	out := make([]drill.Collection, len(rows))
	for idx, r := range rows {
		size, _ := strconv.ParseInt(r[2], 10, 64)
		out[idx] = drill.Collection{Group: db, Name: r[0] + "." + r[1], Bytes: size}
	}
	for _, c := range counted {
		idx, _ := strconv.Atoi(c[0])
		n, _ := strconv.ParseInt(c[1], 10, 64)
		if idx >= 0 && idx < len(out) {
			out[idx].Count = n
		}
	}
	return out, nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func parseEpoch(s string) (time.Time, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, false
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC(), true
}

func (i *instance) Verify(ctx context.Context, inv drill.Inventory) []drill.Check {
	var checks []drill.Check
	if len(inv.Collections) == 0 {
		checks = append(checks, drill.Check{Name: "tables", OK: false, Detail: "the restored cluster holds no user table"})
	} else {
		dbs := map[string]bool{}
		for _, c := range inv.Collections {
			dbs[c.Group] = true
		}
		checks = append(checks, drill.Check{Name: "tables", OK: true, Detail: fmt.Sprintf("%d table(s) across %d database(s), every row counted", len(inv.Collections), len(dbs))})
	}
	out, err := proc.Run(ctx, i.e.cfg.Identity, nil, i.e.tool("pg_amcheck"),
		"--host", i.e.work(), "--port", fmt.Sprint(port), "--username", i.e.cfg.User,
		"--all", "--heapallindexed", "--install-missing", "--no-password")
	if err != nil {
		checks = append(checks, drill.Check{Name: "integrity", OK: false, Detail: err.Error()})
	} else {
		detail := "pg_amcheck: every heap and btree index verified"
		if s := strings.TrimSpace(string(out)); s != "" {
			detail += " (" + s + ")"
		}
		checks = append(checks, drill.Check{Name: "integrity", OK: true, Detail: detail})
	}
	return checks
}

func (i *instance) Stop(ctx context.Context) error {
	_, err := proc.Run(ctx, i.e.cfg.Identity, nil, i.e.tool("pg_ctl"), "-D", i.e.pgdata(), "-m", "fast", "-w", "-t", "120", "stop")
	if !i.e.cfg.KeepData {
		if rerr := os.RemoveAll(i.e.pgdata()); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}
	return err
}

func tailFile(p string, lines int) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}
