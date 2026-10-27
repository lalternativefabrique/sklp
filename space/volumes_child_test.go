package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVolumesToHand_EmptyOnlyAndNeverData(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "fresh")
	full := filepath.Join(root, "pgdata")
	for _, d := range []string{empty, full} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounts := []DevMount{
		{Source: empty, Destination: "/fresh"},
		{Source: full, Destination: "/var/lib/postgresql/data"},
		{Source: "/etc/hosts", Destination: "/etc/hosts"},
	}
	hand, foreign := volumesToHand(mounts, root, 231071)
	if len(hand) != 1 || hand[0] != empty {
		t.Fatalf("hand = %v, want only the empty volume", hand)
	}
	if len(foreign) != 1 || foreign[0] != full {
		t.Fatalf("foreign = %v, want only the volume holding data", foreign)
	}
	hand, foreign = volumesToHand(mounts, root, os.Getuid())
	if len(hand) != 0 || len(foreign) != 0 {
		t.Fatalf("volumes already owned by the wanted uid must be left alone: hand=%v foreign=%v", hand, foreign)
	}
}

func TestVolumesToHand_SidecarNeverTakesAPopulatedVolume(t *testing.T) {
	root := t.TempDir()
	pgdata := filepath.Join(root, "pgdata")
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pgdata, "PG_VERSION"), []byte("16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := []DevMount{{Source: pgdata, Destination: "/var/lib/postgresql/data"}}

	hand, foreign := volumesToHand(shared, root, os.Getuid())
	if len(hand) != 0 || len(foreign) != 0 {
		t.Fatalf("the owner's own start must leave its volume alone: hand=%v foreign=%v", hand, foreign)
	}
	hand, foreign = volumesToHand(shared, root, 231071)
	if len(hand) != 0 {
		t.Fatalf("a sidecar starting as another uid handed %v; a volume with data is never handed over", hand)
	}
	if len(foreign) != 1 || foreign[0] != pgdata {
		t.Fatalf("foreign = %v, want the populated volume reported", foreign)
	}
}
