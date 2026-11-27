package grants

import (
	"os"
	"testing"
)

func TestGrantsRoundTripAndReplace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if g, err := Load(); err != nil || g != nil {
		t.Fatalf("no file: %v, %v", g, err)
	}
	if err := Put(Grant{Space: "playground", Service: "core", Owner: "playground/prod", Scope: Read}); err != nil {
		t.Fatal(err)
	}
	if err := Put(Grant{Space: "playground", Service: "core", Owner: "playground/prod", Scope: Operate}); err != nil {
		t.Fatal(err)
	}
	g, ok := Find("playground", "core")
	if !ok || g.Scope != Operate {
		t.Fatalf("a second grant replaces the first: %+v %v", g, ok)
	}
	if st, err := os.Stat(Path()); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("grants file mode: %v %v", st, err)
	}
	if removed, err := Remove("playground", "core"); !removed || err != nil {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, ok := Find("playground", "core"); ok {
		t.Fatal("removed grant still found")
	}
}

func TestGrantsRefuseWhatCouldEscapeTheirDirectory(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, g := range []Grant{
		{Space: "../etc", Service: "core", Owner: "x", Scope: Read},
		{Space: "ok", Service: "a/b", Owner: "x", Scope: Read},
		{Space: "ok", Service: "core", Owner: "x", Scope: "root"},
		{Space: "ok", Service: "core", Scope: Read},
	} {
		if err := Put(g); err == nil {
			t.Errorf("Put(%+v) accepted", g)
		}
	}
}
