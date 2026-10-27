package spacemanager

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedLauncher blocks every Start issued after arm() until release(), so a
// test can hold the event loop inside a launch while the agent is cancelled.
type gatedLauncher struct {
	*countingExecLauncher
	armed   atomic.Bool
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func newGatedLauncher() *gatedLauncher {
	return &gatedLauncher{countingExecLauncher: newCounting(), entered: make(chan struct{}), gate: make(chan struct{})}
}

func (g *gatedLauncher) Start(ctx context.Context, svc Service) (int, error) {
	if g.armed.Load() {
		g.once.Do(func() { close(g.entered) })
		<-g.gate
	}
	return g.countingExecLauncher.Start(ctx, svc)
}

// Run must outlive the event loop: teardown resets the instance table a
// reconcile in flight is still writing, and a launch that completes after
// teardown leaves a process nobody stops.
func TestRunWaitsForTheEventLoopBeforeTeardown(t *testing.T) {
	l := newGatedLauncher()
	web := Service{Name: "web", Command: []string{"sh", "-c", "sleep 30"}}
	a := NewAgent(l, nil, []Service{web}, testOpts())

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = a.Run(ctx) }()
	waitFor(t, 3*time.Second, "web launched", func() bool { return l.startCount("web-0") >= 1 })

	l.armed.Store(true)
	web.Command = []string{"sh", "-c", "sleep 31"}
	a.Reconfigure(web)
	select {
	case <-l.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the event loop never launched the reconfigured service")
	}

	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned while the event loop was still inside a launch")
	case <-time.After(300 * time.Millisecond):
	}

	close(l.gate)
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return once the launch completed")
	}
}
