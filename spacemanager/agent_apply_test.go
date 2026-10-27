package spacemanager

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newRunningAgent builds an agent over launcher l, runs it, and returns it with
// a cancel + waitgroup. Unlike runAgent it hands back the *Agent so tests can
// call the apply mutators (AddService / RemoveService / ServiceDiffers).
func newRunningAgent(t *testing.T, l Launcher, desired []Service, opts Options) (*Agent, context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := NewAgent(l, nil, desired, opts)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = a.Run(ctx) }()
	return a, cancel, &wg
}

// TestAddServiceStartsIt: AddService registers a new desired service and the
// reconcile loop launches it, exactly like an initial service.
func TestAddServiceStartsIt(t *testing.T) {
	l := newCounting()
	a, cancel, wg := newRunningAgent(t, l, []Service{
		{Name: "api", Command: []string{"sh", "-c", "sleep 5"}},
	}, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "initial service started", func() bool {
		return l.startCount("api-0") >= 1
	})

	a.AddService(Service{Name: "worker", Command: []string{"sh", "-c", "sleep 5"}})

	waitFor(t, 2*time.Second, "added service started", func() bool {
		return l.startCount("worker-0") >= 1
	})
	if !a.HasService("worker") {
		t.Fatal("HasService(worker) false after AddService")
	}
	names := a.DesiredServiceNames()
	if !contains(names, "api") || !contains(names, "worker") {
		t.Fatalf("DesiredServiceNames = %v, want api+worker", names)
	}
}

// TestRemoveServiceTearsDown: RemoveService stops the service's instances and
// drops it from the desired set; reconcile must not relaunch it.
func TestRemoveServiceTearsDown(t *testing.T) {
	rec := &stopRecorder{Launcher: newExecLauncher(), seen: map[string]bool{}}
	a, cancel, wg := newRunningAgent(t, rec, []Service{
		{Name: "api", Command: []string{"sh", "-c", "sleep 30"}},
		{Name: "doomed", Command: []string{"sh", "-c", "sleep 30"}},
	}, testOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 2*time.Second, "doomed started", func() bool {
		return a.State("doomed") == Ready
	})

	a.RemoveService("doomed")

	waitFor(t, 2*time.Second, "doomed stopped", func() bool {
		return rec.wasStopped("doomed-0")
	})
	if a.HasService("doomed") {
		t.Fatal("HasService(doomed) still true after RemoveService")
	}
	// It must not come back: give reconcile a few cycles, then assert it stays
	// out of the desired set.
	time.Sleep(200 * time.Millisecond)
	if contains(a.DesiredServiceNames(), "doomed") {
		t.Fatal("removed service reappeared in desired set")
	}
	if !a.HasService("api") {
		t.Fatal("RemoveService(doomed) wrongly affected api")
	}
}

// TestServiceDiffers: idempotence — an identical spec reports no diff; a changed
// image / replica count / unknown name behave as documented.
func TestServiceDiffers(t *testing.T) {
	a := NewAgent(newExecLauncher(), nil, []Service{
		{Name: "api", Image: "img:1", Replicas: 2, Port: 8080},
	}, testOpts())

	if a.ServiceDiffers(Service{Name: "api", Image: "img:1", Replicas: 2, Port: 8080}) {
		t.Fatal("identical spec reported as differing (not idempotent)")
	}
	if !a.ServiceDiffers(Service{Name: "api", Image: "img:2", Replicas: 2, Port: 8080}) {
		t.Fatal("image change not detected")
	}
	if !a.ServiceDiffers(Service{Name: "api", Image: "img:1", Replicas: 3, Port: 8080}) {
		t.Fatal("replica change not detected")
	}
	if a.ServiceDiffers(Service{Name: "ghost", Image: "x"}) {
		t.Fatal("unknown service reported as differing; want false (it is an add)")
	}
}

func TestServiceDiffers_ReadinessProbe(t *testing.T) {
	base := Service{Name: "worker", Image: "img:1", Ready: &Probe{Log: "Booting Sidekiq", Timeout: "120s"}}
	a := NewAgent(newExecLauncher(), nil, []Service{base}, testOpts())

	if a.ServiceDiffers(Service{Name: "worker", Image: "img:1", Ready: &Probe{Log: "Booting Sidekiq", Timeout: "120s"}}) {
		t.Fatal("an identical probe reported as differing")
	}
	if !a.ServiceDiffers(Service{Name: "worker", Image: "img:1", Ready: &Probe{Log: "Sidekiq", Timeout: "120s"}}) {
		t.Fatal("a corrected readiness probe must be applied, not reported unchanged")
	}
	if !a.ServiceDiffers(Service{Name: "worker", Image: "img:1"}) {
		t.Fatal("dropping the probe must be applied")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
