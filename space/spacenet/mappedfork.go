//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startMapped starts cmd cloned into an EMPTY user namespace and parked on a
// sync pipe until writeMaps has written the child's uid_map/gid_map; closing
// the pipe (EOF) then releases it. The child finds the pipe's fd number in
// envSyncFD and blocks in waitSubidMapping. A process cloned into an empty
// userns holds no capability until it is mapped, which is why the parent has
// to write the maps and why the child re-execs afterwards: caps land at the
// next execve boundary.
//
// cmd.ExtraFiles set by the caller are kept; the pipe takes the next slot.
// Other Cloneflags, Pdeathsig and Setpgid on cmd.SysProcAttr are preserved.
func startMapped(cmd *exec.Cmd, writeMaps func(pid int) error) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("sync pipe: %w", err)
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, pr)
	cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", envSyncFD, 3+len(cmd.ExtraFiles)-1))
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER

	if err := startPinned(cmd); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pr.Close() // the child dup'd its own copy at fork

	mapErr := writeMaps(cmd.Process.Pid)
	_ = pw.Close() // EOF releases the child either way
	if mapErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return mapErr
	}
	return nil
}
