//go:build linux

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair. Done with raw syscalls rather than
// a pty library so the agent keeps no dependency the rest of sklp does not
// already carry.
func openPTY() (ptmx, tty *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w (is devpts mounted?)", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, fmt.Errorf("open pty slave: %w", err)
	}
	return m, s, nil
}

// setWinsize tells the pty how big the host terminal is; full-screen programs
// read this to lay themselves out.
func setWinsize(f *os.File, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Col: uint16(cols),
		Row: uint16(rows),
	})
}
