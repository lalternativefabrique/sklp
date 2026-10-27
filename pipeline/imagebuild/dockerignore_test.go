package imagebuild

import "testing"

func TestDockerignore_Common(t *testing.T) {
	ig := ParseDockerignore(`
# comment
.git
node_modules
*.log
**/secret.key
build/
!build/keep.txt
`)
	cases := []struct {
		path string
		want bool
	}{
		{".git", true},
		{".git/config", true}, // under an ignored dir
		{"node_modules", true},
		{"node_modules/x/y.js", true},
		{"app.log", true},
		{"src/main.go", false},   // not matched
		{"a/b/secret.key", true}, // **/secret.key at depth
		{"secret.key", true},     // **/ matches zero dirs too
		{"build", true},
		{"build/out.bin", true},
		{"build/keep.txt", false}, // re-included by !build/keep.txt (last match)
		{"README.md", false},
	}
	for _, c := range cases {
		if got := ig.Ignored(c.path); got != c.want {
			t.Errorf("Ignored(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestDockerignore_Empty(t *testing.T) {
	ig := ParseDockerignore("")
	if ig.Ignored("anything/at/all") {
		t.Error("empty .dockerignore must ignore nothing")
	}
}

// negation ordering: a later non-negated pattern re-ignores.
func TestDockerignore_LastMatchWins(t *testing.T) {
	ig := ParseDockerignore("*.env\n!keep.env\n*.env\n")
	if !ig.Ignored("keep.env") {
		t.Error("a later *.env should re-ignore keep.env (last match wins)")
	}
}
