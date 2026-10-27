package spec

import "testing"

func TestBranchAllowed(t *testing.T) {
	cases := []struct {
		name   string
		pats   []string
		branch string
		want   bool
	}{
		{"every branch but main, on a feature", []string{"**", "!main"}, "feat/x", true},
		{"every branch but main, on main", []string{"**", "!main"}, "main", false},
		{"main only, on main", []string{"main"}, "main", true},
		{"main only, on a feature", []string{"main"}, "feat/x", false},
		{"bare negation allows the rest", []string{"!main"}, "feat/x", true},
		{"bare negation still denies", []string{"!main"}, "main", false},
		{"doublestar spans slashes", []string{"**"}, "feat/deep/nested", true},
		{"prefix doublestar matches nested", []string{"release/**"}, "release/1/2", true},
		{"prefix doublestar rejects others", []string{"release/**"}, "feat/x", false},
		{"carve an exception out of a prefix", []string{"release/**", "!release/*-rc"}, "release/9-rc", false},
		{"empty list allows everything", nil, "anything", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BranchAllowed(&TriggerBlock{Branches: c.pats}, c.branch); got != c.want {
				t.Errorf("branches %v on %q = %v, want %v", c.pats, c.branch, got, c.want)
			}
		})
	}
}

func TestBranchAllowed_NoContextAllows(t *testing.T) {
	if !BranchAllowed(&TriggerBlock{Branches: []string{"main"}}, "") {
		t.Error("an empty branch means no git context — must not gate")
	}
	if !BranchAllowed(nil, "main") {
		t.Error("a nil trigger allows every branch")
	}
}

func TestBranchAllowed_LegacySpelling(t *testing.T) {
	if BranchAllowed(&TriggerBlock{Never: true}, "main") {
		t.Error("never: true still keeps a pipeline out")
	}
	if BranchAllowed(&TriggerBlock{Except: []string{"main"}}, "main") {
		t.Error("except: [main] still denies main")
	}
	if !BranchAllowed(&TriggerBlock{Except: []string{"main"}}, "feat/x") {
		t.Error("except: [main] still allows a feature branch")
	}
}

func TestBranchAllowed_BranchesWinsOverLegacy(t *testing.T) {
	trig := &TriggerBlock{Branches: []string{"main"}, Never: true}
	if !BranchAllowed(trig, "main") {
		t.Error("an explicit branches list is the new spelling and must win over never:")
	}
}
