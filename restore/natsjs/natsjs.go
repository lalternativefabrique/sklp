package natsjs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/restore/objstore"
	"github.com/lalternativefabrique/sklp/restore/proc"
)

type Config struct {
	Store     objstore.Store
	Bucket    string
	Prefix    string
	Scratch   string
	ServerBin string
	Ready     time.Duration
}

type Engine struct {
	cfg      Config
	store    string
	onDisk   map[string][]string
	accounts accountBackup
	users    map[string]string
}

func New(cfg Config) *Engine {
	if cfg.ServerBin == "" {
		cfg.ServerBin = "nats-server"
	}
	if cfg.Ready == 0 {
		cfg.Ready = 10 * time.Minute
	}
	return &Engine{cfg: cfg}
}

func (e *Engine) Kind() string   { return "nats" }
func (e *Engine) Target() string { return e.cfg.Bucket + "/" + e.cfg.Prefix }

const (
	storeArchive   = "jetstream-store"
	accountArchive = "jetstream-account"
)

// Locate picks the newest archive by upload time, `nats account backup`
// output or a copy of the store: the names carry dates in several formats
// across apps, the object timestamp is the one truth.
func (e *Engine) Locate(ctx context.Context) (drill.Backup, error) {
	objs, err := e.cfg.Store.List(ctx, e.cfg.Prefix)
	if err != nil {
		return drill.Backup{}, err
	}
	var newest *objstore.Object
	for i, o := range objs {
		name := path.Base(o.Key)
		if !strings.HasPrefix(name, "nats-") || !strings.HasSuffix(name, ".tar.gz") {
			continue
		}
		if newest == nil || o.Modified.After(newest.Modified) {
			newest = &objs[i]
		}
	}
	if newest == nil {
		return drill.Backup{}, fmt.Errorf("%w: no nats-*.tar.gz under %s", drill.ErrNoBackup, e.Target())
	}
	kind := storeArchive
	if strings.HasPrefix(path.Base(newest.Key), accountArchivePrefix) {
		kind = accountArchive
	}
	return drill.Backup{Label: newest.Key, Type: kind, TakenAt: newest.Modified.UTC(), Bytes: newest.Size}, nil
}

func (e *Engine) prepareAccountRestore() error {
	backup, err := readAccountBackup(filepath.Join(e.root(), "data"))
	if err != nil {
		return err
	}
	e.accounts, e.onDisk = backup, backup.streams()
	e.users = map[string]string{}
	for acct := range backup {
		if acct == globalAccount {
			continue
		}
		if e.users[acct], err = randomPassword(); err != nil {
			return err
		}
	}
	e.store = filepath.Join(e.root(), "store")
	return os.MkdirAll(e.store, 0o700)
}

func (e *Engine) root() string { return filepath.Join(e.cfg.Scratch, "nats") }

func (e *Engine) Restore(ctx context.Context, b drill.Backup) error {
	if err := os.RemoveAll(e.root()); err != nil {
		return err
	}
	if err := os.MkdirAll(e.root(), 0o700); err != nil {
		return err
	}
	rc, err := e.cfg.Store.Open(ctx, b.Label)
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := extract(rc, filepath.Join(e.root(), "data")); err != nil {
		return err
	}
	e.accounts, e.users = nil, nil
	if b.Type == accountArchive {
		return e.prepareAccountRestore()
	}
	if e.store, err = storeDir(filepath.Join(e.root(), "data")); err != nil {
		return err
	}
	e.onDisk, err = onDisk(e.store)
	return err
}

type instance struct {
	cmd     *exec.Cmd
	done    chan error
	monitor string
	onDisk  map[string][]string
	root    string
	log     string
}

func (e *Engine) Start(ctx context.Context) (drill.Instance, error) {
	port, err := proc.FreePort()
	if err != nil {
		return nil, err
	}
	mport, err := proc.FreePort()
	if err != nil {
		return nil, err
	}
	conf := filepath.Join(e.root(), "server.conf")
	confText := serverConf(port, mport, e.store, e.onDisk)
	if e.accounts != nil {
		confText = accountServerConf(port, mport, e.store, e.users)
	}
	if err := os.WriteFile(conf, []byte(confText), 0o600); err != nil {
		return nil, err
	}
	logPath := filepath.Join(e.root(), "server.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(e.cfg.ServerBin, "-c", conf)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("start %s: %w", e.cfg.ServerBin, err)
	}
	inst := &instance{cmd: cmd, done: make(chan error, 1), monitor: fmt.Sprintf("http://127.0.0.1:%d", mport), onDisk: e.onDisk, root: e.root(), log: logPath}
	go func() { inst.done <- cmd.Wait(); logFile.Close() }()

	if err := inst.waitHealthy(ctx, e.cfg.Ready); err != nil {
		_ = inst.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	if e.accounts != nil {
		if err := restoreAccounts(fmt.Sprintf("nats://127.0.0.1:%d", port), e.accounts, e.users); err != nil {
			_ = inst.Stop(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	return inst, nil
}

func serverConf(port, mport int, store string, accounts map[string][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen: 127.0.0.1:%d\nhttp: 127.0.0.1:%d\nserver_name: restore-drill\n", port, mport)
	fmt.Fprintf(&b, "jetstream {\n  store_dir: %q\n  max_memory_store: 64MB\n}\n", store)
	var named []string
	for a := range accounts {
		if a != "$G" {
			named = append(named, a)
		}
	}
	sort.Strings(named)
	if len(named) > 0 {
		b.WriteString("accounts {\n")
		for _, a := range named {
			fmt.Fprintf(&b, "  %q: { jetstream: enabled }\n", a)
		}
		b.WriteString("}\n")
	}
	return b.String()
}

func (i *instance) waitHealthy(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last string
	for time.Now().Before(deadline) {
		select {
		case err := <-i.done:
			i.done <- err
			return fmt.Errorf("nats-server exited while loading the store (%v): %s", err, tailFile(i.log, 15))
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		status, body, err := i.get(ctx, "/healthz")
		if err == nil && status == http.StatusOK {
			return nil
		}
		last = fmt.Sprintf("%d %s %v", status, strings.TrimSpace(string(body)), err)
	}
	return fmt.Errorf("nats-server not healthy after %s: %s", budget, last)
}

func (i *instance) get(ctx context.Context, p string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.monitor+p, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, body, err
}

type jsz struct {
	Accounts []struct {
		Name    string `json:"name"`
		Streams []struct {
			Name  string `json:"name"`
			State struct {
				Messages  int64     `json:"messages"`
				Bytes     int64     `json:"bytes"`
				FirstSeq  uint64    `json:"first_seq"`
				LastSeq   uint64    `json:"last_seq"`
				LastTS    time.Time `json:"last_ts"`
				Consumers int       `json:"consumer_count"`
			} `json:"state"`
		} `json:"stream_detail"`
	} `json:"account_details"`
}

func (i *instance) Inspect(ctx context.Context) (drill.Inventory, error) {
	status, body, err := i.get(ctx, "/jsz?accounts=true&streams=true")
	if err != nil {
		return drill.Inventory{}, err
	}
	if status != http.StatusOK {
		return drill.Inventory{}, fmt.Errorf("/jsz answered %d", status)
	}
	var j jsz
	if err := json.Unmarshal(body, &j); err != nil {
		return drill.Inventory{}, fmt.Errorf("decode /jsz: %w", err)
	}
	var inv drill.Inventory
	for _, a := range j.Accounts {
		for _, s := range a.Streams {
			c := drill.Collection{Group: a.Name, Name: s.Name, Count: s.State.Messages, Bytes: s.State.Bytes}
			if !s.State.LastTS.IsZero() && s.State.LastTS.Year() > 1 {
				ts := s.State.LastTS.UTC()
				c.Newest = &ts
			}
			inv.Bytes += s.State.Bytes
			inv.Collections = append(inv.Collections, c)
		}
	}
	return inv, nil
}

func (i *instance) Verify(ctx context.Context, inv drill.Inventory) []drill.Check {
	loaded := map[string]bool{}
	for _, c := range inv.Collections {
		loaded[c.Group+"/"+c.Name] = true
	}
	var missing []string
	total := 0
	for acct, streams := range i.onDisk {
		for _, s := range streams {
			total++
			if !loaded[acct+"/"+s] {
				missing = append(missing, acct+"/"+s)
			}
		}
	}
	sort.Strings(missing)

	checks := []drill.Check{}
	switch {
	case total == 0:
		checks = append(checks, drill.Check{Name: "streams", OK: true, Detail: "the store restores and holds no stream: the server had none when it was backed up"})
	case len(missing) > 0:
		checks = append(checks, drill.Check{Name: "streams", OK: false,
			Detail: fmt.Sprintf("%d of %d stream(s) in the archive did not load: %s", len(missing), total, strings.Join(missing, ", "))})
	default:
		checks = append(checks, drill.Check{Name: "streams", OK: true, Detail: fmt.Sprintf("all %d stream(s) in the archive loaded", total)})
	}

	status, body, err := i.get(ctx, "/healthz?details=true")
	switch {
	case err != nil:
		checks = append(checks, drill.Check{Name: "health", OK: false, Detail: err.Error()})
	case status != http.StatusOK:
		checks = append(checks, drill.Check{Name: "health", OK: false, Detail: strings.TrimSpace(string(body))})
	default:
		checks = append(checks, drill.Check{Name: "health", OK: true, Detail: "JetStream reports every stream and consumer healthy"})
	}
	return checks
}

func (i *instance) Stop(ctx context.Context) error {
	defer os.RemoveAll(i.root)
	select {
	case err := <-i.done:
		i.done <- err
		return nil
	default:
	}
	_ = i.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-i.done:
		return nil
	case <-time.After(30 * time.Second):
		_ = i.cmd.Process.Kill()
		<-i.done
		return errors.New("nats-server ignored SIGTERM and was killed")
	case <-ctx.Done():
		_ = i.cmd.Process.Kill()
		return ctx.Err()
	}
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
