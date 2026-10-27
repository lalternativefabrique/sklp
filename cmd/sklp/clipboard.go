package main

import (
	"errors"
	"io"
	"os/exec"
	"strings"
)

// clipboardTools are the copy-out commands tried in order. Wayland first (the
// cockpit's primary target is a modern Linux desktop), then X11, then macOS.
var clipboardTools = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
}

// errNoClipboardTool reports that none of clipboardTools is on PATH.
var errNoClipboardTool = errors.New("no clipboard tool on PATH (install wl-clipboard, xclip or xsel)")

// copyToClipboard pipes s into the first available clipboard command. Injected as
// cockpit.Clipboard so the cockpit model stays free of process execution.
func copyToClipboard(s string) error {
	for _, tool := range clipboardTools {
		path, err := exec.LookPath(tool[0])
		if err != nil {
			continue
		}
		cmd := exec.Command(path, tool[1:]...)
		cmd.Stdin = strings.NewReader(s)
		// The cockpit owns the alt-screen; a tool writing to the inherited stdio
		// would paint over it.
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Run(); err != nil {
			return err
		}
		return nil
	}
	return errNoClipboardTool
}
