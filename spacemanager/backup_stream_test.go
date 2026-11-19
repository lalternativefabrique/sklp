package spacemanager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/objstore"
)

type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	prior   []objstore.Object
	putErr  error
}

func (m *memStore) List(_ context.Context, prefix string) ([]objstore.Object, error) {
	var out []objstore.Object
	for _, o := range m.prior {
		if strings.HasPrefix(o.Key, prefix) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	if m.putErr != nil {
		return 0, m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[key] = b
	return int64(len(b)), nil
}

type streamRunner struct {
	*fakeRunner
	output []byte
	code   int
	err    error
}

func (r *streamRunner) run(_ context.Context, _ ExecTarget, _ []string, stdout io.Writer) (int, error) {
	if _, err := stdout.Write(r.output); err != nil {
		return -1, err
	}
	return r.code, r.err
}

var gzipped = append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("x"), 1000)...)

func streamedSvc() Service {
	return Service{
		Name:   "postgres",
		Backup: &ServiceBackup{Command: []string{"sh", "-c", "pg_dump | gzip"}, Schedule: "0 3 * * *"},
		Env: []string{
			"SKLP_BACKUP_TO=https://s3.example/bucket/postgres",
			"AWS_ACCESS_KEY_ID=ak", "AWS_SECRET_ACCESS_KEY=sk", "AWS_DEFAULT_REGION=gra",
		},
	}
}

func streamScheduler(t *testing.T, r *streamRunner, store *memStore) (*backupScheduler, *[]BackupOutcome, *objstore.S3Config) {
	t.Helper()
	r.fakeRunner = &fakeRunner{ok: true}
	r.setDesired(streamedSvc())
	sched := newBackupScheduler(r.fakeRunner, r.run)
	var cfg objstore.S3Config
	sched.openStore = func(c objstore.S3Config) (archiveStore, error) { cfg = c; return store, nil }
	var outcomes []BackupOutcome
	sched.record = func(o BackupOutcome) { outcomes = append(outcomes, o) }
	return sched, &outcomes, &cfg
}

var at3 = time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)

func TestBackupStream_ShipsStdoutUnderTheDeclaredPrefix(t *testing.T) {
	store := &memStore{}
	sched, outcomes, cfg := streamScheduler(t, &streamRunner{output: gzipped}, store)
	sched.tick(context.Background(), at3)

	want := "postgres/postgres-2026-11-19T030000Z.gz"
	if got := store.objects[want]; !bytes.Equal(got, gzipped) {
		t.Fatalf("object %s = %d bytes, want the command's stdout; have %v", want, len(got), store.objects)
	}
	if o := (*outcomes)[0]; !o.OK || o.Key != want || o.Bytes != int64(len(gzipped)) {
		t.Errorf("outcome = %+v", o)
	}
	if cfg.Bucket != "bucket" || cfg.Endpoint != "https://s3.example" || cfg.Region != "gra" || cfg.AccessKey != "ak" {
		t.Errorf("s3 config = %+v", *cfg)
	}
}

func TestBackupStream_FailingCommandPublishesNothing(t *testing.T) {
	store := &memStore{}
	sched, outcomes, _ := streamScheduler(t, &streamRunner{output: gzipped, code: 1}, store)
	sched.tick(context.Background(), at3)

	if len(store.objects) != 0 {
		t.Errorf("a failed dump must not be published, got %v", store.objects)
	}
	if o := (*outcomes)[0]; o.OK || !strings.Contains(o.Error, "exited 1") {
		t.Errorf("outcome = %+v", o)
	}
}

func TestBackupStream_EmptyOutputFails(t *testing.T) {
	store := &memStore{}
	sched, outcomes, _ := streamScheduler(t, &streamRunner{}, store)
	sched.tick(context.Background(), at3)

	if len(store.objects) != 0 {
		t.Errorf("nothing to publish, got %v", store.objects)
	}
	if o := (*outcomes)[0]; o.OK || o.Error != errNoOutput.Error() {
		t.Errorf("outcome = %+v", o)
	}
}

func TestBackupStream_UploadFailureFails(t *testing.T) {
	store := &memStore{putErr: errors.New("s3 down")}
	sched, outcomes, _ := streamScheduler(t, &streamRunner{output: gzipped}, store)
	sched.tick(context.Background(), at3)

	if o := (*outcomes)[0]; o.OK || o.Error != "s3 down" {
		t.Errorf("outcome = %+v", o)
	}
}

func TestBackupStream_WarnsWhenTheArchiveShrinks(t *testing.T) {
	store := &memStore{prior: []objstore.Object{
		{Key: "postgres/postgres-2026-11-18T030000Z.gz", Size: 10_000, Modified: at3.Add(-24 * time.Hour)},
		{Key: "postgres/other-2026-11-18T030000Z.gz", Size: 1, Modified: at3.Add(-time.Hour)},
	}}
	sched, outcomes, _ := streamScheduler(t, &streamRunner{output: gzipped}, store)
	sched.tick(context.Background(), at3)

	if o := (*outcomes)[0]; !o.OK || !strings.Contains(o.Warning, "previous one was 10000") {
		t.Errorf("outcome = %+v", o)
	}
}

func TestBackupStream_WithoutDestinationKeepsTheCommandAsIs(t *testing.T) {
	fr := &fakeRunner{ok: true, exitCode: 2}
	sched := schedulerOver(fr, backupSvc("db", "0 3 * * *"))
	sched.openStore = func(objstore.S3Config) (archiveStore, error) {
		t.Fatal("no SKLP_BACKUP_TO: nothing to stream to")
		return nil, nil
	}
	var outcomes []BackupOutcome
	sched.record = func(o BackupOutcome) { outcomes = append(outcomes, o) }
	sched.tick(context.Background(), at3)

	if len(outcomes) != 1 || outcomes[0].OK || outcomes[0].Error != "exited 2" {
		t.Errorf("outcomes = %+v", outcomes)
	}
}
