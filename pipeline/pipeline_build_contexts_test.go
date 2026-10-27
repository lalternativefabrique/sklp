package pipeline

import "testing"

// TestLocalDirBuildContext_RecognisesPaths confirms the classifier
// treats bare paths (relative or absolute) as local-dir sources that
// the runner should hash into the cache key.
func TestLocalDirBuildContext_RecognisesPaths(t *testing.T) {
	cases := []string{
		"../platform",
		"./vendor",
		"vendor",
		"/abs/path/to/dir",
		"a/b/c",
	}
	for _, c := range cases {
		got, ok := localDirBuildContext(c)
		if !ok {
			t.Errorf("local-dir %q misclassified as remote", c)
		}
		if got != c {
			t.Errorf("local-dir %q: classifier mutated value to %q", c, got)
		}
	}
}

// TestLocalDirBuildContext_SkipsRemoteSchemes is the negative side:
// every BuildKit-resolved source must short-circuit so it does not get
// folded into the cache key as a tree (the value ref is hashed verbatim
// via the spec's JSON marshalling).
func TestLocalDirBuildContext_SkipsRemoteSchemes(t *testing.T) {
	cases := []string{
		"docker-image://digstack/schemas:v1.2.0",
		"docker-image://digstack/schemas@sha256:abc",
		"oci-layout:///var/tmp/layout",
		"target:api",
		"https://github.com/foo/bar.git#v1.0",
		"http://example.com/tarball.tar.gz",
		"git://gitlab.com/foo/bar.git",
		"ssh://git@github.com/foo/bar.git",
	}
	for _, c := range cases {
		if _, ok := localDirBuildContext(c); ok {
			t.Errorf("remote %q misclassified as local-dir", c)
		}
	}
}

// TestLocalDirBuildContext_EmptyIsNotLocal guards against a "" value
// being silently classified as a local-dir hash root (which would mean
// hashing the repo root). compilePublish also rejects empty values
// up front; this is the second line of defence.
func TestLocalDirBuildContext_EmptyIsNotLocal(t *testing.T) {
	if _, ok := localDirBuildContext(""); ok {
		t.Errorf("empty value misclassified as local-dir")
	}
}
