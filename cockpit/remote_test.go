package cockpit

import (
	"context"
	"errors"
	"testing"
)

func withRemoteNodes(t *testing.T, nodes []RemoteNode) {
	t.Helper()
	RemoteNodeList = func(ctx context.Context) []RemoteNode { return nodes }
	t.Cleanup(func() { RemoteNodeList = nil })
}

// A remote node's spaces are listed alongside the local ones, tagged with the
// node they run on.
func TestRemoteSpacesAreListed(t *testing.T) {
	withRemoteNodes(t, []RemoteNode{{
		Name:   "altfab-vm",
		Target: "https://10.0.0.4:6464",
		Spaces: []SpaceSummary{{Name: "production", Status: "running", Runc: []ContainerSummary{{ID: "web-0", Status: "running"}}}},
	}})

	got := loadRemoteSpaces(context.Background(), "altfab-vm")
	if len(got) != 1 {
		t.Fatalf("want 1 remote space, got %d", len(got))
	}
	if got[0].Node != "altfab-vm" || got[0].Name != "production" {
		t.Fatalf("row not tagged with its node: %+v", got[0])
	}
}

// An unreachable node stays VISIBLE with its error: a machine that dropped out
// of the list would read as "nothing deployed there".
func TestUnreachableNodeIsListedWithItsError(t *testing.T) {
	withRemoteNodes(t, []RemoteNode{{
		Name:   "prod-shop",
		Target: "https://10.0.0.9:6464",
		Err:    errors.New("dial tcp: i/o timeout"),
	}})

	got := loadRemoteSpaces(context.Background(), "prod-shop")
	if len(got) != 1 {
		t.Fatalf("a failed node must still produce a row, got %d", len(got))
	}
	if got[0].Node != "prod-shop" || got[0].Err == "" {
		t.Fatalf("row must carry the node and its error: %+v", got[0])
	}
}

// The destructive keys act on the LOCAL registry, so a remote row must be
// refused: a same-named local space would be torn down instead.
func TestLocalActionsRefuseRemoteSpaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  Key
	}{
		{"delete", KeyKill},
		{"stop", KeyStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			KillSpace = func(string) error { t.Fatal("local teardown fired for a remote space"); return nil }
			StopSpace = func(string) error { t.Fatal("local stop fired for a remote space"); return nil }
			t.Cleanup(func() { KillSpace, StopSpace = nil, nil })

			m := &Model{Spaces: []Space{{Name: "production", Node: "altfab-vm", Containers: 1}}}
			m.Update(tc.key)
			if m.PendingKill != "" {
				t.Fatalf("a remote space must not arm a local teardown (kill=%q)", m.PendingKill)
			}
			if !contains(m.Note, "altfab-vm") {
				t.Fatalf("note=%q must name the node the space runs on", m.Note)
			}
		})
	}
}

// A local teardown must not drop a same-named space belonging to another node.
func TestRemoveSpaceKeepsRemoteHomonyms(t *testing.T) {
	m := &Model{Spaces: []Space{
		{Name: "production"},
		{Name: "production", Node: "altfab-vm"},
	}}
	m.removeSpace("production")
	if len(m.Spaces) != 1 {
		t.Fatalf("want the remote homonym kept, got %+v", m.Spaces)
	}
	if m.Spaces[0].Node != "altfab-vm" {
		t.Fatalf("the wrong row was removed: %+v", m.Spaces[0])
	}
}

// Purge counts only what its seam can reap, so remote rows never inflate the
// confirmation prompt.
func TestPurgeIgnoresRemoteSpaces(t *testing.T) {
	PurgeSpaces = func() ([]string, error) { t.Fatal("purge fired with no local space"); return nil, nil }
	t.Cleanup(func() { PurgeSpaces = nil })

	m := &Model{Spaces: []Space{{Name: "production", Node: "altfab-vm", Volumes: []string{"pgdata"}}}}
	if n := m.idleVolumeCount(); n != 0 {
		t.Fatalf("remote volumes counted in the purge prompt: %d", n)
	}
	m.Update(KeyPurge)
	if m.PendingPurge {
		t.Fatal("a list of only remote spaces must not arm a purge")
	}
}

// Opening logs on a remote space must target that node's control API, and must
// NOT fall through to the local CI-log store: a file on this machine says
// nothing about a space running elsewhere.
func TestRemoteLogsTargetTheNode(t *testing.T) {
	CILogStream = func(ctx context.Context, repo, task string, onLine func(string)) (string, error) {
		t.Fatal("a remote space must not read the LOCAL CI log store")
		return "", nil
	}
	t.Cleanup(func() { CILogStream = nil })

	m := &Model{Spaces: []Space{{
		Name: "production", Node: "altfab-vm", Supervised: true, LimitedInfo: true,
		Services: []Service{{Name: "web", State: StateReady}},
	}}}
	m.ToggleLogs()

	if m.LogsCI {
		t.Fatal("remote logs must not be served from the local CI store")
	}
	if m.LogsNode != "altfab-vm" || m.LogsSpace != "production" {
		t.Fatalf("panel not scoped to the node: node=%q space=%q", m.LogsNode, m.LogsSpace)
	}
	if m.LogsService != "web" {
		t.Fatalf("LogsService = %q, want the space's service", m.LogsService)
	}
}

// A remote space with no control API cannot be streamed, and the message must
// name the machine rather than implying a local problem.
func TestRemoteLogsWithoutControlAPI(t *testing.T) {
	m := &Model{Spaces: []Space{{Name: "dev-1", Node: "altfab-vm", LimitedInfo: true}}}
	m.ToggleLogs()
	if !contains(m.LogsErr, "altfab-vm") {
		t.Fatalf("LogsErr = %q, want it to name the node", m.LogsErr)
	}
}
