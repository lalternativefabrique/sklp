package spacemanager

import (
	"strings"
	"testing"
)

// A database nobody backs up is the one failure with no recovery, so the node
// must name it rather than deploy in silence.
func TestUnbackedVolumes_NamesWhatNothingCovers(t *testing.T) {
	got := UnbackedVolumes([]Service{
		{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
		{Name: "nats", Volumes: []string{"jetstream:/data"}},
	})
	if len(got) != 2 {
		t.Fatalf("UnbackedVolumes = %v, want both volumes reported", got)
	}
	if !strings.Contains(strings.Join(got, " "), "postgres:pgdata") {
		t.Errorf("the report does not name the service and its volume: %v", got)
	}
}

// A service that declares a backup is covered, so reporting it would train the
// operator to ignore the warning.
func TestUnbackedVolumes_SkipsABackedService(t *testing.T) {
	got := UnbackedVolumes([]Service{{
		Name:    "postgres",
		Volumes: []string{"pgdata:/var/lib/postgresql/data"},
		Backup:  &ServiceBackup{Command: []string{"pg_dump"}, Schedule: "0 * * * *"},
	}})
	if len(got) != 0 {
		t.Errorf("UnbackedVolumes = %v, want nothing for a backed service", got)
	}
}

// A host bind is a location the operator chose and may already back up from
// outside sklp; only named volumes are sklp's to speak for.
func TestUnbackedVolumes_IgnoresHostBinds(t *testing.T) {
	got := UnbackedVolumes([]Service{
		{Name: "app", Volumes: []string{"/srv/app/data:/data"}},
	})
	if len(got) != 0 {
		t.Errorf("UnbackedVolumes = %v, want host binds ignored", got)
	}
}

// A stateless space has nothing to lose and must produce no noise.
func TestUnbackedVolumes_QuietForAStatelessSpace(t *testing.T) {
	if got := UnbackedVolumes([]Service{{Name: "web"}, {Name: "api"}}); len(got) != 0 {
		t.Errorf("UnbackedVolumes = %v, want nothing for a stateless space", got)
	}
}

// The reported name is the volume's NAME, not its container mount point: that
// is what an operator writes in a backup command.
func TestUnbackedVolumes_ReportsTheVolumeName(t *testing.T) {
	got := UnbackedVolumes([]Service{
		{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
	})
	if len(got) != 1 || got[0] != "postgres:pgdata" {
		t.Errorf("UnbackedVolumes = %v, want [postgres:pgdata]", got)
	}
}
