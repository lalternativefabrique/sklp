package control

import (
	"context"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// A pre-resolved spec never re-parses YAML, so the apply edge itself must
// refuse a host bind: a client holding the token could otherwise mount the
// node's own config dir into a container.
func TestHandleApply_HostBindVolumeRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		ready:   true,
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	svc := spacemanager.Service{Name: "www", Volumes: []string{"/home/architect/.config/sklp/control:/loot:ro"}}
	spec, err := MarshalApplySpec([]spacemanager.Service{svc}, spacemanager.SpaceMeta{Name: "production", Stack: "www"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	_, err = c.Apply(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "host paths cannot be mounted") {
		t.Fatalf("want host-bind refusal, got %v", err)
	}
	if len(sup.live) != 0 {
		t.Fatalf("refused apply must not add services, got %v", sup.live)
	}
}
