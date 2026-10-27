package tools

import "testing"

func TestKubectlURL(t *testing.T) {
	k := &kubectl{}
	cases := []struct {
		v, os, arch, want string
	}{
		{"1.32.0", "linux", "amd64", "https://dl.k8s.io/release/v1.32.0/bin/linux/amd64/kubectl"},
		{"1.31.5", "darwin", "arm64", "https://dl.k8s.io/release/v1.31.5/bin/darwin/arm64/kubectl"},
	}
	for _, c := range cases {
		got, err := k.URL(c.v, c.os, c.arch)
		if err != nil {
			t.Errorf("(%s, %s, %s): %v", c.v, c.os, c.arch, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s, %s, %s): got %s, want %s", c.v, c.os, c.arch, got, c.want)
		}
	}
}

func TestKubectlName(t *testing.T) {
	if (&kubectl{}).Name() != "kubectl" {
		t.Fatal("kubectl.Name() must return \"kubectl\"")
	}
}
