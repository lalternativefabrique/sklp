package spawn

import (
	"strings"
	"testing"
)

func TestResolveImage(t *testing.T) {
	s := &OCISpawner{Image: "registry.example.com/acme/sklp-runner-base:abc"}
	cases := []struct {
		kind, want string
		wantErr    bool
	}{
		{"", "registry.example.com/acme/sklp-runner-base:abc", false},
		{"base", "registry.example.com/acme/sklp-runner-base:abc", false},
		{"ops", "registry.example.com/acme/sklp-runner-ops:abc", false},
		{"registry.example.com/acme/custom:1", "registry.example.com/acme/custom:1", false},
		{"docker.io/evil:latest", "", true},
	}
	for _, c := range cases {
		got, err := s.resolveImage(c.kind)
		if (err != nil) != c.wantErr {
			t.Fatalf("resolveImage(%q): err=%v wantErr=%v", c.kind, err, c.wantErr)
		}
		if got != c.want {
			t.Errorf("resolveImage(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestHostMountFor(t *testing.T) {
	s := &OCISpawner{WorkspaceHost: "/var/lib/ci"}
	got, err := s.hostMountFor("/tmp/foo")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/var/lib/ci/foo" {
		t.Errorf("rebase = %q, want /var/lib/ci/foo", got)
	}

	bare := &OCISpawner{}
	got, _ = bare.hostMountFor("/home/u/repo")
	if got != "/home/u/repo" {
		t.Errorf("no WorkspaceHost = %q, want verbatim", got)
	}

	if _, err := bare.hostMountFor("relative/path"); err == nil {
		t.Error("expected error for non-absolute repo path")
	}
}

func TestApiKeyEnvNames(t *testing.T) {
	if got := apiKeyEnvNames("ovh"); strings.Join(got, ",") != "OVH_KEY,OVH_API_KEY" {
		t.Errorf("ovh = %v", got)
	}
	if got := apiKeyEnvNames("scaleway"); strings.Join(got, ",") != "SCW_SECRET_KEY" {
		t.Errorf("scaleway = %v", got)
	}
	if got := apiKeyEnvNames("custom"); got[0] != "CUSTOM_API_KEY" {
		t.Errorf("fallback = %v", got)
	}
}

func TestSwapImageNameBareRef(t *testing.T) {
	if got := swapImageName("sklp-runner-base:dev", "sklp-runner-ops"); got != "sklp-runner-ops:dev" {
		t.Errorf("bare swap = %q", got)
	}
}
