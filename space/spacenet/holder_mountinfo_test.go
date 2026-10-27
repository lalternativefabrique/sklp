//go:build linux

package spacenet

import "testing"

func TestOverlayMountedAt(t *testing.T) {
	const merged = "/home/u/.cache/sklp/spaces/x/overlay/web/merged"
	empty := "22 1 0:21 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"
	mounted := empty + "640 22 0:55 / " + merged + " rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n"
	other := empty + "640 22 0:55 / " + merged + " rw,relatime - tmpfs tmpfs rw\n"
	sibling := empty + "640 22 0:55 / " + merged + "2 rw,relatime - overlay overlay rw\n"

	if overlayMountedAt([]byte(empty), merged) {
		t.Fatal("no mount reported as mounted")
	}
	if !overlayMountedAt([]byte(mounted), merged) {
		t.Fatal("overlay at merged not detected")
	}
	if overlayMountedAt([]byte(other), merged) {
		t.Fatal("non-overlay fs at merged accepted")
	}
	if overlayMountedAt([]byte(sibling), merged) {
		t.Fatal("overlay at a sibling path accepted")
	}
	spaced := "640 22 0:55 / /a\\040b/merged rw - overlay overlay rw\n"
	if !overlayMountedAt([]byte(spaced), "/a b/merged") {
		t.Fatal("escaped space in mount point not matched")
	}
}
