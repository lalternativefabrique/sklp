package cockpit

import "testing"

func stoppedDevSpace() Space {
	return Space{
		Name:        "sklp-dev-3aa69aa11788",
		Status:      statusStopped,
		Repo:        "/home/dev/skalpai",
		LimitedInfo: true,
	}
}

func withRestartSeam(t *testing.T) {
	t.Helper()
	prev := RestartDev
	RestartDev = func(string, string) error { return nil }
	t.Cleanup(func() { RestartDev = prev })
}

func TestRestartArmsOnAStoppedDevSpace(t *testing.T) {
	withRestartSeam(t)
	m := &Model{Spaces: []Space{stoppedDevSpace()}}
	if !m.Update(KeyRestart) {
		t.Fatal("KeyRestart should act on a stopped dev space")
	}
	req := m.TakePendingRestart()
	if req == nil {
		t.Fatal("no restart armed")
	}
	if req.Space != "sklp-dev-3aa69aa11788" || req.Repo != "/home/dev/skalpai" {
		t.Fatalf("request = %+v, want the selected space and its repo", req)
	}
	if m.TakePendingRestart() != nil {
		t.Fatal("one keypress must arm exactly one restart")
	}
}

func TestRestartRefusesARunningSpace(t *testing.T) {
	withRestartSeam(t)
	sp := stoppedDevSpace()
	sp.Status = "running"
	sp.Containers = 3
	m := &Model{Spaces: []Space{sp}}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("a running space has nothing to relaunch")
	}
	if m.Note == "" {
		t.Fatal("the refusal must be explained")
	}
}

func TestRestartRefusesANonDevSpace(t *testing.T) {
	withRestartSeam(t)
	sp := stoppedDevSpace()
	sp.Name = "sklp-ci-abc123"
	m := &Model{Spaces: []Space{sp}}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("only `sklp dev` spaces can be relaunched")
	}
}

func TestRestartRefusesASupervisedDeploy(t *testing.T) {
	withRestartSeam(t)
	sp := stoppedDevSpace()
	sp.Supervised = true
	sp.LimitedInfo = false
	m := &Model{Spaces: []Space{sp}}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("a supervised deploy has its own lifecycle")
	}
}

func TestRestartRefusesASpaceWithNoKnownRepo(t *testing.T) {
	withRestartSeam(t)
	sp := stoppedDevSpace()
	sp.Repo = ""
	m := &Model{Spaces: []Space{sp}}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("without a repo there is no directory to run `sklp dev` in")
	}
	if m.Note == "" {
		t.Fatal("the refusal must be explained")
	}
}

func TestRestartReportsAMissingSeam(t *testing.T) {
	prev := RestartDev
	RestartDev = nil
	t.Cleanup(func() { RestartDev = prev })
	m := &Model{Spaces: []Space{stoppedDevSpace()}}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("no seam, no restart")
	}
	if m.Note == "" {
		t.Fatal("an unavailable action must say so")
	}
}

func TestRestartIsInertInsideTheLogsPanel(t *testing.T) {
	withRestartSeam(t)
	m := &Model{Spaces: []Space{stoppedDevSpace()}, LogsOpen: true, SearchMatch: -1}
	m.Update(KeyRestart)
	if m.PendingRestart != nil {
		t.Fatal("the panel owns the screen; restart belongs to the spaces list")
	}
}

func TestIsDevSpace(t *testing.T) {
	for name, want := range map[string]bool{
		"sklp-dev-3aa69aa11788": true,
		"sklp-ci-3aa69aa11788":  false,
		"shop-production":       false,
	} {
		if got := IsDevSpace(name); got != want {
			t.Errorf("IsDevSpace(%q) = %v, want %v", name, got, want)
		}
	}
}
