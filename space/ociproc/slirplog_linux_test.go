//go:build linux

package ociproc

import (
	"strings"
	"sync"
	"testing"
)

func TestSlirpLogCollapsesToOneLine(t *testing.T) {
	var b SlirpLog
	b.Write([]byte("sent tapfd=6 for tap0\nreceived tapfd=6\nStarting slirp\n"))
	got := b.String()
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("String() must stay on one line so it cannot move a live block's cursor, got %q", got)
	}
	if got != "sent tapfd=6 for tap0 received tapfd=6 Starting slirp" {
		t.Fatalf("unexpected collapse: %q", got)
	}
}

func TestSlirpLogEmptyStillNamesTheSuspects(t *testing.T) {
	var b SlirpLog
	if got := b.String(); got == "" {
		t.Fatal("an empty log must still give the caller something actionable")
	}
}

func TestSlirpLogConcurrentWrites(t *testing.T) {
	var b SlirpLog
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Write([]byte("x"))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = b.String()
	}()
	wg.Wait()
	if n := strings.Count(b.String(), "x"); n != 50 {
		t.Fatalf("lost writes under concurrency: got %d, want 50", n)
	}
}
