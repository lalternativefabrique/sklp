package spacemanager

import (
	"fmt"
	"testing"
)

// A merge that only rewrites DeclaredImage must not read as a spec change: it
// would bounce a healthy service.
func TestSameSpec_IgnoresDeclaredImage(t *testing.T) {
	a := Service{Name: "web", Image: "rg/web:2", DeclaredImage: "rg/web:1"}
	b := Service{Name: "web", Image: "rg/web:2", DeclaredImage: "rg/web:2"}
	if !SameSpec(a, b) {
		t.Fatal("DeclaredImage alone reported a spec change")
	}
	if !SameSpec(Service{Name: "web", Image: "rg/web:2"}, b) {
		t.Fatal("an unset DeclaredImage reported a spec change")
	}
}

func TestSameSpec_IgnoresRolledImages(t *testing.T) {
	a := Service{Name: "web", Image: "rg/web:2", RolledImages: []string{"rg/web:2"}}
	b := Service{Name: "web", Image: "rg/web:2"}
	if !SameSpec(a, b) {
		t.Fatal("RolledImages alone reported a spec change")
	}
}

func TestJournalRolledImage(t *testing.T) {
	j := JournalRolledImage(nil, "a")
	j = JournalRolledImage(j, "b")
	j = JournalRolledImage(j, "a")
	if len(j) != 2 || j[0] != "a" || j[1] != "b" {
		t.Fatalf("journal = %v, want [a b]", j)
	}
	for i := 0; i < maxRolledImages+5; i++ {
		j = JournalRolledImage(j, fmt.Sprintf("t%d", i))
	}
	if len(j) != maxRolledImages || j[0] != "t5" {
		t.Fatalf("journal len %d first %q, want %d / t5", len(j), j[0], maxRolledImages)
	}
}
