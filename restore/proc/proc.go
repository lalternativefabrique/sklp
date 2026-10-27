package proc

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Identity is the unprivileged user an engine's binaries run as. Postgres
// refuses to start as root, and a drill has no reason to hold root anyway.
type Identity struct {
	UID, GID uint32
	Name     string
	Home     string
}

func Lookup(name string) (*Identity, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("running as root but no %q user to drop to: %w", name, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	return &Identity{UID: uint32(uid), GID: uint32(gid), Name: u.Username, Home: u.HomeDir}, nil
}

func (id *Identity) Own(paths ...string) error {
	if id == nil {
		return nil
	}
	for _, p := range paths {
		if err := os.Chown(p, int(id.UID), int(id.GID)); err != nil {
			return err
		}
	}
	return nil
}

func (id *Identity) OwnTree(root string) error {
	if id == nil {
		return nil
	}
	return filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(id.UID), int(id.GID))
	})
}

func Command(ctx context.Context, id *Identity, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	if id != nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: id.UID, Gid: id.GID}}
		cmd.Env = append(cmd.Env, "HOME="+id.Home, "USER="+id.Name)
	}
	return cmd
}

// Run executes a command and folds its combined output into the error, so a
// failed step says what the tool said rather than just an exit status.
func Run(ctx context.Context, id *Identity, env []string, name string, args ...string) ([]byte, error) {
	var out bytes.Buffer
	cmd := Command(ctx, id, env, name, args...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, tail(out.String(), 20))
	}
	return out.Bytes(), nil
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func FreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
