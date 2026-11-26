package spacemanager

import (
	"testing"
	"time"
)

// gatedStopLauncher holds every Stop until release is closed, the way a real
// teardown takes its time killing a container and its overlay holder.
type gatedStopLauncher struct {
	*countingExecLauncher
	stopping chan struct{}
	release  chan struct{}
}

func (s *gatedStopLauncher) Stop(id string) {
	select {
	case s.stopping <- struct{}{}:
	default:
	}
	<-s.release
	s.countingExecLauncher.Stop(id)
}

// A relaunch reuses the instance id, and with it the overlay holder the
// launcher keys by that id: starting before the previous run's Stop lands lets
// that Stop tear down the new run's holder, and runc then fails to enter it.
func TestRestartRelaunchWaitsForThePreviousStop(t *testing.T) {
	l := &gatedStopLauncher{countingExecLauncher: newCounting(), stopping: make(chan struct{}, 1), release: make(chan struct{})}
	desired := []Service{{Name: "web", Command: []string{"sh", "-c", "sleep 5"}}}
	a, cancel, wg := startExecAgent(t, l, desired, testOpts())
	defer wg.Wait()
	defer cancel()
	defer func() {
		select {
		case <-l.release:
		default:
			close(l.release)
		}
	}()

	waitFor(t, 3*time.Second, "initial launch", func() bool { return l.startCount("web-0") >= 1 })
	before := l.startCount("web-0")

	if !a.RestartService("web") {
		t.Fatal("RestartService(web) = false")
	}
	select {
	case <-l.stopping:
	case <-time.After(3 * time.Second):
		t.Fatal("the bounce never reached Stop")
	}

	time.Sleep(500 * time.Millisecond)
	if got := l.startCount("web-0"); got != before {
		t.Fatalf("web-0 relaunched %d time(s) while its previous Stop was still running", got-before)
	}

	close(l.release)
	waitFor(t, 3*time.Second, "web relaunched once the stop landed", func() bool {
		return l.startCount("web-0") > before
	})
}
