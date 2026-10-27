package tools

import "testing"

func TestHelmURL(t *testing.T) {
	h := &helm{}
	got, err := h.URL("3.16.0", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://get.helm.sh/helm-v3.16.0-linux-amd64.tar.gz"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestHelmName(t *testing.T) {
	if (&helm{}).Name() != "helm" {
		t.Fatal("helm.Name() must return \"helm\"")
	}
}
