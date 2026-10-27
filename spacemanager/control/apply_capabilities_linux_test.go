package control

import (
	"context"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// A pre-resolved spec never re-parses YAML, so the apply edge itself refuses a
// capability that reaches past the userns.
func TestHandleApply_HostReachingCapabilityRefused(t *testing.T) {
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

	svc := spacemanager.Service{Name: "www", Capabilities: []string{"CAP_DAC_READ_SEARCH"}}
	spec, err := MarshalApplySpec([]spacemanager.Service{svc}, spacemanager.SpaceMeta{Name: "production", Stack: "www"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	_, err = c.Apply(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "cannot be granted by a manifest") {
		t.Fatalf("want capability refusal, got %v", err)
	}
	if len(sup.live) != 0 {
		t.Fatalf("refused apply must not add services, got %v", sup.live)
	}
}
