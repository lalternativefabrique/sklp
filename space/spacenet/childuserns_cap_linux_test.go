//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// A capability held inside a child-userns space is scoped to that space's own
// ids: it cannot reach an operator file or another space's file. DAC_OVERRIDE
// is the sharpest case — it bypasses the file permission bits — yet the kernel
// still refuses it against a uid the child userns does not map. This is the
// property the manifest capability allowlist relies on.
func TestChildUserns_DACOverrideStaysInsideTheSpace(t *testing.T) {
	cn := requireChildUserns(t)
	defer cn.Close()

	// A file owned by the operator (outside any space's mapped range), 0600.
	operatorSecret := filepath.Join(t.TempDir(), "operator-secret")
	if err := os.WriteFile(operatorSecret, []byte("node-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	openExecutableToOthers(t) // path traversal for the entered process

	l1, u1, w1, m1 := overlayDirs(t, "c0")
	l2, u2, w2, m2 := overlayDirs(t, "c1")
	p1, _, err := cn.SpawnOverlayChild(l1, u1, w1, m1, 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := cn.SpawnOverlayChild(l2, u2, w2, m2, 1)
	if err != nil {
		t.Fatal(err)
	}
	waitOverlayMounted(t, p1, m1)
	waitOverlayMounted(t, p2, m2)

	// Enter space 1's userns AS ITS ROOT and grant the entering shell the
	// full bounding set, so DAC_OVERRIDE is effective there — the strongest a
	// manifest grant could ever produce.
	enterCap := func(pid int, sh string) (string, error) {
		args := append(ociproc.EnterArgs(pid, true), "--",
			"capsh", "--caps=cap_dac_override+eip", "--", "-c", sh)
		out, err := exec.Command("nsenter", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// Space 2 writes a 0600 file only its own root owns.
	w2args := append(append(ociproc.EnterArgs(p2, true), "--"), "sh", "-c", "echo s > "+m2+"/secret && chmod 600 "+m2+"/secret")
	if _, err := exec.Command("nsenter", w2args...).CombinedOutput(); err != nil {
		t.Fatalf("write in space 2: %v", err)
	}

	// Space 1, even wielding DAC_OVERRIDE, reads neither the operator's file
	// nor space 2's: both owners are unmapped in its userns, so the capability
	// has no authority over them.
	if out, _ := enterCap(p1, "cat "+operatorSecret+" >/dev/null 2>&1 && echo READ || echo denied"); out != "denied" {
		t.Fatalf("DAC_OVERRIDE in space 1 read the operator's file: %q", out)
	}
	if out, _ := enterCap(p1, "cat "+filepath.Join(u2, "secret")+" >/dev/null 2>&1 && echo READ || echo denied"); out != "denied" {
		t.Fatalf("DAC_OVERRIDE in space 1 read space 2's file: %q", out)
	}
	// Sanity: the same capability DOES let it read a 0000 file it owns itself,
	// so the test is exercising DAC_OVERRIDE, not merely a missing file.
	own := fmt.Sprintf("%s/mine", m1)
	if out, err := enterCap(p1, "echo x > "+own+" && chmod 000 "+own+" && cat "+own+" >/dev/null 2>&1 && echo READ || echo denied"); err != nil || out != "READ" {
		t.Fatalf("DAC_OVERRIDE should read the space's own 0000 file: out=%q err=%v", out, err)
	}
}
