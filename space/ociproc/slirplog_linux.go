//go:build linux

package ociproc

import (
	"bytes"
	"strings"
	"sync"
)

// SlirpLog collects a slirp4netns process's output off the terminal. Both of
// its streams are written by the child while a parent goroutine reads the
// buffer on the startup-failure path, so the mutex is load-bearing.
type SlirpLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SlirpLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the collected output collapsed onto a single line so it can be
// embedded in an error without breaking the caller's formatting.
func (b *SlirpLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.TrimSpace(b.buf.String())
	if s == "" {
		return "no output; check /dev/net/tun and capabilities"
	}
	return strings.Join(strings.Fields(s), " ")
}
