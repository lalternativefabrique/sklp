package spacemanager

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/lalternativefabrique/sklp/spacemanager/wire"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/restore/objstore"
)

type BackupOutcome = wire.BackupOutcome

type archiveStore interface {
	List(ctx context.Context, prefix string) ([]objstore.Object, error)
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
}

// streamTarget is where a `backup:` command's stdout is shipped when the
// service declares SKLP_BACKUP_TO: the archive never touches the node's disk.
type streamTarget struct {
	dest backupDestination
	s3   objstore.S3Config
}

func streamTargetFor(svc Service) (*streamTarget, bool) {
	dest, ok := declaredBackupDestination(svc)
	if !ok {
		return nil, false
	}
	if dest.Prefix != "" && !strings.HasSuffix(dest.Prefix, "/") {
		dest.Prefix += "/"
	}
	env := envMap(svc.Env)
	return &streamTarget{dest: dest, s3: objstore.S3Config{
		Endpoint:  dest.Endpoint,
		Region:    firstNonEmpty(env["AWS_DEFAULT_REGION"], env["AWS_REGION"]),
		Bucket:    dest.Bucket,
		AccessKey: env["AWS_ACCESS_KEY_ID"],
		SecretKey: env["AWS_SECRET_ACCESS_KEY"],
	}}, true
}

func openS3Store(cfg objstore.S3Config) (archiveStore, error) { return objstore.NewS3(cfg) }

var errNoOutput = errors.New("the backup command wrote nothing to stdout")

// shrinkRatio flags an archive smaller than this fraction of the previous one:
// a dump cut short by a full disk or a dropped connection still exits 0 when
// its pipeline lacks pipefail, and its size is the only trace left.
const shrinkRatio = 0.5

func (s *backupScheduler) stream(ctx context.Context, e backupEntry, target ExecTarget, now time.Time) BackupOutcome {
	out := BackupOutcome{Service: e.service, At: now.UTC()}
	store, err := s.openStore(e.stream.s3)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	namePrefix := e.stream.dest.Prefix + e.service + "-"
	previous := newestSize(ctx, store, e.stream.dest.Prefix, namePrefix)

	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		code, err := s.run(ctx, target, e.command, pw)
		if err == nil && code != 0 {
			err = fmt.Errorf("the backup command exited %d", code)
		}
		pw.CloseWithError(err)
		execDone <- err
	}()

	br := bufio.NewReaderSize(pr, 64<<10)
	head, peekErr := br.Peek(2)
	if len(head) == 0 {
		pr.CloseWithError(errNoOutput)
		if execErr := <-execDone; execErr != nil {
			out.Error = execErr.Error()
		} else if peekErr != nil && peekErr != io.EOF {
			out.Error = peekErr.Error()
		} else {
			out.Error = errNoOutput.Error()
		}
		return out
	}

	key := namePrefix + out.At.Format("2006-01-02T150405Z") + archiveExt(head)
	n, putErr := store.Put(ctx, key, br)
	if putErr != nil {
		pr.CloseWithError(putErr)
	}
	execErr := <-execDone
	switch {
	case execErr != nil:
		out.Error = execErr.Error()
	case putErr != nil:
		out.Error = putErr.Error()
	default:
		out.OK, out.Key, out.Bytes = true, key, n
		if previous > 0 && float64(n) < shrinkRatio*float64(previous) {
			out.Warning = fmt.Sprintf("archive is %d bytes, the previous one was %d: check it is complete", n, previous)
		}
	}
	return out
}

func newestSize(ctx context.Context, store archiveStore, listPrefix, namePrefix string) int64 {
	objs, err := store.List(ctx, listPrefix)
	if err != nil {
		return 0
	}
	var newest objstore.Object
	for _, o := range objs {
		if strings.HasPrefix(o.Key, namePrefix) && o.Modified.After(newest.Modified) {
			newest = o
		}
	}
	return newest.Size
}

func archiveExt(head []byte) string {
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return ".gz"
	}
	return ""
}

type backupLog struct {
	mu   sync.Mutex
	last map[string]BackupOutcome
}

func (a *Agent) RecordBackup(o BackupOutcome) {
	a.backups.mu.Lock()
	defer a.backups.mu.Unlock()
	if a.backups.last == nil {
		a.backups.last = map[string]BackupOutcome{}
	}
	a.backups.last[o.Service] = o
}

func (a *Agent) LastBackups() map[string]BackupOutcome {
	a.backups.mu.Lock()
	defer a.backups.mu.Unlock()
	out := make(map[string]BackupOutcome, len(a.backups.last))
	for k, v := range a.backups.last {
		out[k] = v
	}
	return out
}

// WithLastBackups attaches each store's last scheduled backup outcome.
func WithLastBackups(stores []DataStore, last map[string]BackupOutcome) []DataStore {
	for i := range stores {
		if o, ok := last[stores[i].Service]; ok {
			stores[i].LastBackup = &o
		}
	}
	return stores
}
