package natsjs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/restore/objstore"
	"github.com/lalternativefabrique/sklp/restore/proc"
)

func serverBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("NATS_SERVER_BIN"); p != "" {
		return p
	}
	p, err := exec.LookPath("nats-server")
	if err != nil {
		t.Skip("nats-server not installed; set NATS_SERVER_BIN")
	}
	return p
}

// seedStore runs a real server over dir and publishes n messages into a
// stream, speaking the text protocol so the test needs no client library.
func seedStore(t *testing.T, bin, dir string, n int) {
	t.Helper()
	port, _ := proc.FreePort()
	cmd := exec.Command(bin, "-js", "-sd", dir, "-a", "127.0.0.1", "-p", fmt.Sprint(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	}()
	var conn net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if conn, err = net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	send := func(s string) {
		if _, err := conn.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(prefix string) string {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("waiting for %s: %v", prefix, err)
			}
			if strings.HasPrefix(line, "PING") {
				send("PONG\r\n")
				continue
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
	}
	expect("INFO")
	send("CONNECT {\"verbose\":false,\"pedantic\":false}\r\nSUB _INBOX.t 1\r\n")
	create := `{"name":"EVENTS","subjects":["events.>"],"storage":"file"}`
	send(fmt.Sprintf("PUB $JS.API.STREAM.CREATE.EVENTS _INBOX.t %d\r\n%s\r\n", len(create), create))
	expect("MSG")
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`{"n":%d}`, i)
		send(fmt.Sprintf("PUB events.created _INBOX.t %d\r\n%s\r\n", len(body), body))
		expect("MSG")
	}
}

func tarGz(t *testing.T, src, dst string, extra ...tar.Header) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		info, _ := d.Info()
		hdr, _ := tar.FileInfoHeader(info, "")
		hdr.Name = "./" + filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			_, err = tw.Write(b)
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range extra {
		h := h
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDrillRestoresEveryMessageFromTheNewestArchive(t *testing.T) {
	bin := serverBin(t)
	data := t.TempDir()
	seedStore(t, bin, data, 25)

	bucket := t.TempDir()
	tarGz(t, data, filepath.Join(bucket, "nats", "nats-2026-09-26T0300.tar.gz"))
	old := filepath.Join(bucket, "nats", "nats-2026-09-20T0300.tar.gz")
	if err := os.WriteFile(old, []byte("not even gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-6 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)
	if err := os.WriteFile(filepath.Join(bucket, "nats", "nats-account-20260926T030000Z.tar.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(bucket, "nats", "nats-account-20260926T030000Z.tar.gz"), past, past)

	eng := New(Config{Store: objstore.Dir(bucket), Bucket: "app-backups", Prefix: "nats/", Scratch: t.TempDir(), ServerBin: bin, Ready: time.Minute})
	rep := drill.Run(context.Background(), eng, drill.Options{Space: "app-production", MaxRPO: time.Hour})

	var out strings.Builder
	rep.Render(&out)
	if rep.Status != drill.StatusOK {
		t.Fatalf("drill failed:\n%s", out.String())
	}
	if rep.Backup.Label != "nats/nats-2026-09-26T0300.tar.gz" {
		t.Fatalf("restored %s, want the newest store archive", rep.Backup.Label)
	}
	if len(rep.Inventory.Collections) != 1 {
		t.Fatalf("collections = %+v", rep.Inventory.Collections)
	}
	c := rep.Inventory.Collections[0]
	if c.Name != "EVENTS" || c.Count != 25 || c.Newest == nil {
		t.Fatalf("stream = %+v, want EVENTS with all 25 messages", c)
	}
	if _, err := os.Stat(filepath.Join(eng.cfg.Scratch, "nats")); !os.IsNotExist(err) {
		t.Fatal("the restored data was left on disk")
	}
}

func TestDrillFindsAStoreMountedOneLevelDown(t *testing.T) {
	bin := serverBin(t)
	data := t.TempDir()
	seedStore(t, bin, filepath.Join(data, "restored"), 3)
	bucket := t.TempDir()
	tarGz(t, data, filepath.Join(bucket, "nats-2026-09-26.tar.gz"))

	eng := New(Config{Store: objstore.Dir(bucket), Scratch: t.TempDir(), ServerBin: bin, Ready: time.Minute})
	rep := drill.Run(context.Background(), eng, drill.Options{})
	if rep.Status != drill.StatusOK || rep.Inventory.Collections[0].Count != 3 {
		t.Fatalf("status %s, failures %v", rep.Status, rep.Failures())
	}
}

func TestDrillFailsAnArchiveWithoutJetStream(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "readme"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bucket := t.TempDir()
	tarGz(t, src, filepath.Join(bucket, "nats-2026-09-26.tar.gz"))
	rep := drill.Run(context.Background(), New(Config{Store: objstore.Dir(bucket), Scratch: t.TempDir()}), drill.Options{})
	if rep.Status != drill.StatusFailed || !strings.Contains(rep.Summary(), "no jetstream directory") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestDrillRefusesAnEntryEscapingTheScratchDir(t *testing.T) {
	src := t.TempDir()
	bucket := t.TempDir()
	tarGz(t, src, filepath.Join(bucket, "nats-2026-09-26.tar.gz"),
		tar.Header{Name: "../../escaped", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0})
	rep := drill.Run(context.Background(), New(Config{Store: objstore.Dir(bucket), Scratch: t.TempDir()}), drill.Options{})
	if rep.Status != drill.StatusFailed || !strings.Contains(rep.Summary(), "escapes the restore directory") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestDrillPassesAnEmptyStore(t *testing.T) {
	bin := serverBin(t)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "jetstream"), 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := t.TempDir()
	tarGz(t, data, filepath.Join(bucket, "nats-2026-09-27.tar.gz"))
	eng := New(Config{Store: objstore.Dir(bucket), Scratch: t.TempDir(), ServerBin: bin, Ready: time.Minute})
	rep := drill.Run(context.Background(), eng, drill.Options{})
	if rep.Status != drill.StatusOK || len(rep.Inventory.Collections) != 0 {
		t.Fatalf("status %s, failures %v", rep.Status, rep.Failures())
	}
}

func TestDrillReportsNoBackup(t *testing.T) {
	rep := drill.Run(context.Background(), New(Config{Store: objstore.Dir(t.TempDir()), Scratch: t.TempDir()}), drill.Options{})
	if rep.Status != drill.StatusFailed || !strings.Contains(rep.Summary(), "no backup found") {
		t.Fatalf("summary: %s", rep.Summary())
	}
}

func TestServerConfDeclaresNamedAccounts(t *testing.T) {
	conf := serverConf(1, 2, "/s", map[string][]string{"$G": {"A"}, "gate": {"EVENTS"}})
	if !strings.Contains(conf, `"gate": { jetstream: enabled }`) || strings.Contains(conf, `"$G"`) {
		t.Fatalf("conf:\n%s", conf)
	}
}
