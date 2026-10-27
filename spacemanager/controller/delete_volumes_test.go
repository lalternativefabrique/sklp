package controller

import (
	"context"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// The delete listener is what runSpace must prefer once it is set: an agent
// that wires it would otherwise register a handle with no volume-dropping
// teardown, and the node would answer every delete by keeping the data.
func TestWrapListenersSignalsReadyForTheDeleteListener(t *testing.T) {
	var called, ready bool
	opts := wrapListeners(SpaceOptions{
		NetListenerWithDelete: func(context.Context, string, control.Supervisor, func(), func(), func()) { called = true },
	}, func(error) { ready = true })
	if opts.NetListenerWithDelete == nil {
		t.Fatal("the delete listener must stay the one runSpace calls")
	}
	opts.NetListenerWithDelete(context.Background(), "s", nil, func() {}, func() {}, func() {})
	if !called || !ready {
		t.Fatalf("called=%v ready=%v, want both: AddSpace waits on this signal", called, ready)
	}
}

// A delete is a purge that also takes the volumes: it must latch BOTH intents,
// since the cleanup only runs at all when the purge one is set.
func TestShutdownAndDeleteLatchesPurgeToo(t *testing.T) {
	var purged, deleted bool
	opts := SpaceOptions{}
	opts.ShutdownAndPurge = func() { purged = true }
	opts.ShutdownAndDelete = func() {
		deleted = true
		opts.ShutdownAndPurge()
	}

	opts.ShutdownAndDelete()

	if !deleted || !purged {
		t.Fatalf("deleted=%v purged=%v, want both: the cleanup only runs on a purge", deleted, purged)
	}
}
