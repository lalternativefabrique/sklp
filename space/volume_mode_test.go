package space

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTakeImageModes_NewbornVolumeGetsTheImageMode(t *testing.T) {
	image := t.TempDir()
	imgDir := filepath.Join(image, "var", "lib", "postgresql")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(imgDir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	vols := t.TempDir()
	born := filepath.Join(vols, "pgdata")
	kept := filepath.Join(vols, "old")
	for _, d := range []string{born, kept} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []DevMount{
		{Source: born, Destination: "/var/lib/postgresql"},
		{Source: kept, Destination: "/var/lib/postgresql"},
		{Source: filepath.Join(vols, "none"), Destination: "/not/in/image"},
	}
	takeImageModes([]string{born}, mounts, image)

	st, _ := os.Stat(born)
	if got := st.Mode() & (os.ModePerm | os.ModeSticky); got != 0o777|os.ModeSticky {
		t.Fatalf("newborn volume mode = %v, want the image's 1777", got)
	}
	st, _ = os.Stat(kept)
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("a volume that is not newborn was changed: %v", st.Mode())
	}
}

func TestTakeImageModes_VolumeOverAPathTheImageLacksIsOpenLikeDocker(t *testing.T) {
	image := t.TempDir()
	born := filepath.Join(t.TempDir(), "git_data")
	if err := os.Mkdir(born, 0o700); err != nil {
		t.Fatal(err)
	}
	takeImageModes([]string{born}, []DevMount{{Source: born, Destination: "/data"}}, image)

	st, _ := os.Stat(born)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("newborn volume over a path absent from the image = %v, want docker's 0755", st.Mode().Perm())
	}
}

func TestEmptyOwnedVolumes_NewbornOwnedBySpaceUserCounts(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "git_data")
	full := filepath.Join(root, "pgdata")
	for _, d := range []string{empty, full} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "PG_VERSION"), []byte("16"), 0o600); err != nil {
		t.Fatal(err)
	}
	mounts := []DevMount{{Source: empty, Destination: "/data"}, {Source: full, Destination: "/var/lib/postgresql/data"}, {Source: "/etc/hosts", Destination: "/etc/hosts"}}

	got := emptyOwnedVolumes(mounts, root, os.Getuid())
	if len(got) != 1 || got[0] != empty {
		t.Fatalf("emptyOwnedVolumes = %v, want only the empty volume %s", got, empty)
	}
}
