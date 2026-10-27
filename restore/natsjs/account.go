package natsjs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	accountArchivePrefix = "nats-account-"
	globalAccount        = "$G"
	snapshotMeta         = "backup.json"
	restoreChunk         = 64 * 1024
)

var snapshotData = []string{"stream.arc.s2", "stream.tar.s2"}

// accountBackup is what `nats account backup` writes: one directory per
// stream, at the top for a single-account server, or under one directory per
// backup user when the server holds several accounts.
type accountBackup map[string]map[string]string

func readAccountBackup(root string) (accountBackup, error) {
	base := root
	if entries, err := os.ReadDir(root); err == nil && len(entries) == 1 && entries[0].IsDir() {
		if _, err := os.Stat(filepath.Join(root, entries[0].Name(), snapshotMeta)); errors.Is(err, os.ErrNotExist) {
			base = filepath.Join(root, entries[0].Name())
		}
	}
	out := accountBackup{}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != snapshotMeta {
			return nil
		}
		dir := filepath.Dir(p)
		rel, _ := filepath.Rel(base, dir)
		parts := strings.Split(rel, string(os.PathSeparator))
		account, stream := globalAccount, parts[len(parts)-1]
		switch len(parts) {
		case 1:
		case 2:
			account = parts[0]
		default:
			return fmt.Errorf("stream backup %q sits deeper than an account directory", rel)
		}
		if out[account] == nil {
			out[account] = map[string]string{}
		}
		out[account][stream] = dir
		return nil
	})
	if err != nil {
		return nil, err
	}
	if out[globalAccount] != nil && len(out) > 1 {
		return nil, errors.New("the archive mixes top-level streams with per-account directories")
	}
	return out, nil
}

func (a accountBackup) streams() map[string][]string {
	out := map[string][]string{}
	for acct, streams := range a {
		for s := range streams {
			out[acct] = append(out[acct], s)
		}
		sort.Strings(out[acct])
	}
	return out
}

func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func accountServerConf(port, mport int, store string, users map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "listen: 127.0.0.1:%d\nhttp: 127.0.0.1:%d\nserver_name: restore-drill\n", port, mport)
	fmt.Fprintf(&b, "jetstream {\n  store_dir: %q\n  max_memory_store: 64MB\n}\n", store)
	if len(users) == 0 {
		return b.String()
	}
	names := make([]string, 0, len(users))
	for a := range users {
		names = append(names, a)
	}
	sort.Strings(names)
	b.WriteString("accounts {\n")
	for _, a := range names {
		fmt.Fprintf(&b, "  %q: { jetstream: enabled, users: [{ user: %q, password: %q }] }\n", a, a, users[a])
	}
	b.WriteString("}\n")
	return b.String()
}

type restoreRequest struct {
	Config struct {
		Name    string `json:"name"`
		Storage string `json:"storage"`
	} `json:"config"`
}

type apiResponse struct {
	Error *struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"error,omitempty"`
	DeliverSubject string `json:"deliver_subject,omitempty"`
}

func (r apiResponse) err() error {
	if r.Error == nil {
		return nil
	}
	return fmt.Errorf("%s (%d)", r.Error.Description, r.Error.Code)
}

// restoreStream speaks the JetStream restore API the way `nats stream
// restore` does: the stream's saved config and state open the restore, the s2
// archive follows in acknowledged chunks, and an empty message closes it.
func restoreStream(nc *nats.Conn, stream, dir string) error {
	meta, err := os.ReadFile(filepath.Join(dir, snapshotMeta))
	if err != nil {
		return err
	}
	var req restoreRequest
	if err := json.Unmarshal(meta, &req); err != nil {
		return fmt.Errorf("%s: %w", snapshotMeta, err)
	}
	if req.Config.Name != stream {
		return fmt.Errorf("%s names stream %q, not %q", snapshotMeta, req.Config.Name, stream)
	}
	if req.Config.Storage == "memory" {
		return nil
	}
	var data string
	for _, name := range snapshotData {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			data = filepath.Join(dir, name)
			break
		}
	}
	if data == "" {
		return errors.New("no stream archive beside " + snapshotMeta)
	}

	msg, err := nc.Request("$JS.API.STREAM.RESTORE."+stream, meta, 30*time.Second)
	if err != nil {
		return err
	}
	var open apiResponse
	if err := json.Unmarshal(msg.Data, &open); err != nil {
		return err
	}
	if err := open.err(); err != nil {
		return err
	}

	f, err := os.Open(data)
	if err != nil {
		return err
	}
	defer f.Close()
	chunk := make([]byte, restoreChunk)
	for {
		n, rerr := f.Read(chunk)
		if n > 0 {
			ack, err := nc.Request(open.DeliverSubject, chunk[:n], 30*time.Second)
			if err != nil {
				return err
			}
			if len(ack.Data) > 0 {
				var r apiResponse
				if json.Unmarshal(ack.Data, &r) == nil && r.err() != nil {
					return r.err()
				}
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	done, err := nc.Request(open.DeliverSubject, nil, time.Hour)
	if err != nil {
		return err
	}
	var final apiResponse
	if err := json.Unmarshal(done.Data, &final); err != nil {
		return err
	}
	return final.err()
}

func restoreAccounts(url string, backup accountBackup, users map[string]string) error {
	accounts := make([]string, 0, len(backup))
	for a := range backup {
		accounts = append(accounts, a)
	}
	sort.Strings(accounts)
	for _, acct := range accounts {
		opts := []nats.Option{nats.Name("restore-drill")}
		if pw, ok := users[acct]; ok {
			opts = append(opts, nats.UserInfo(acct, pw))
		}
		nc, err := nats.Connect(url, opts...)
		if err != nil {
			return fmt.Errorf("connect as %s: %w", acct, err)
		}
		streams := make([]string, 0, len(backup[acct]))
		for s := range backup[acct] {
			streams = append(streams, s)
		}
		sort.Strings(streams)
		for _, s := range streams {
			if err := restoreStream(nc, s, backup[acct][s]); err != nil {
				nc.Close()
				return fmt.Errorf("restore %s/%s: %w", acct, s, err)
			}
		}
		nc.Close()
	}
	return nil
}
