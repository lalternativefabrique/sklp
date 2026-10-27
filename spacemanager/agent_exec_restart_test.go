package spacemanager

import (
	"context"
	"sync"
	"testing"
	"time"
)

// execProviderLauncher wraps the counting exec launcher with an ExecCoords
// method so the agent's ResolveExecTarget can resolve a live instance without a
// real runc (the coords are synthetic but unique per instance id).
type execProviderLauncher struct {
	*countingExecLauncher
}

func (e *execProviderLauncher) ExecCoords(id string) (string, string, string) {
	return "/fake/runc", "/fake/root", "sklp-deploy-" + id
}

// startExecAgent constructs and runs an Agent over l, returning the live agent plus
// a cancel/wait pair. Unlike runAgent it hands back the *Agent so the test can
// call ResolveExecTarget / RestartService on the running instance.
func startExecAgent(t *testing.T, l Launcher, desired []Service, opts Options) (*Agent, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := NewAgent(l, nil, desired, opts)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = a.Run(ctx)
	}()
	return a, cancel, &wg
}

// TestResolveExecTargetReadyInstance: once a service is running, the agent
// resolves the exec coordinates of its live instance; an unknown service is not
// resolvable, and a launcher without ExecCoords cannot exec at all.
func TestResolveExecTargetReadyInstance(t *testing.T) {
	l := &execProviderLauncher{newCounting()}
	desired := []Service{{Name: "web", Command: []string{"sh", "-c", "sleep 5"}}}
	a, cancel, wg := startExecAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	var target ExecTarget
	waitFor(t, 3*time.Second, "web instance resolvable for exec", func() bool {
		tg, ok := a.ResolveExecTarget("web")
		if ok {
			target = tg
		}
		return ok
	})
	if target.ContainerID != "sklp-deploy-web-0" || target.RuncBin != "/fake/runc" {
		t.Fatalf("exec target = %+v", target)
	}

	if _, ok := a.ResolveExecTarget("ghost"); ok {
		t.Fatal("unknown service must not resolve an exec target")
	}
}

// TestResolveExecTargetNoExecProvider: a launcher that does not implement
// ExecCoords (the plain exec test launcher) can never resolve an exec target.
func TestResolveExecTargetNoExecProvider(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "web", Command: []string{"sh", "-c", "sleep 5"}}}
	a, cancel, wg := startExecAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	// Even after the instance is live, no exec provider => not resolvable.
	waitFor(t, 3*time.Second, "web started", func() bool { return l.startCount("web-0") >= 1 })
	if _, ok := a.ResolveExecTarget("web"); ok {
		t.Fatal("launcher without ExecCoords must not resolve an exec target")
	}
}

// TestRestartServiceBounces: RestartService stops the live instances and the
// reconcile loop relaunches them, so the per-instance Start count grows. An
// unknown service returns false and starts nothing new.
func TestRestartServiceBounces(t *testing.T) {
	l := newCounting()
	desired := []Service{{Name: "web", Command: []string{"sh", "-c", "sleep 5"}}}
	a, cancel, wg := startExecAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 3*time.Second, "initial launch", func() bool { return l.startCount("web-0") >= 1 })
	before := l.startCount("web-0")

	if ok := a.RestartService("web"); !ok {
		t.Fatal("RestartService(web) = false, want true for a known service")
	}
	waitFor(t, 3*time.Second, "web relaunched after restart", func() bool {
		return l.startCount("web-0") > before
	})

	if ok := a.RestartService("ghost"); ok {
		t.Fatal("RestartService(ghost) = true, want false for an unknown service")
	}
}
