package runnerbuild

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// On the host the proc mount must be strict: swallowing its failure let every
// Go build die later with "cannot find GOROOT directory", an error naming the
// toolchain rather than the mount that actually failed.
func TestProcMountIsCreatedAndStrictOnTheHost(t *testing.T) {
	script := UnshareStepRunner{}.buildScript("/rootfs", []string{"go build ./..."})

	mountAt := strings.Index(script, "mount -t proc")
	mkdirAt := strings.Index(script, "mkdir -p")
	if mountAt < 0 {
		t.Fatal("/proc must be mounted: a trimmed go resolves its GOROOT through /proc/self/exe")
	}
	if mkdirAt < 0 || mkdirAt > mountAt {
		t.Fatal("the mountpoint must be created first: a base image need not ship /proc")
	}

	line := script[mountAt:]
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if strings.Contains(line, "|| true") || strings.Contains(line, "2>/dev/null") {
		t.Fatalf("the proc mount must not be best-effort; failing it silently defers the error to an unrelated tool:\n%s", line)
	}
}

// Nested inside the OCI runner mount(2) cannot succeed at all: CAP_SYS_ADMIN
// is absent from the bounding set and noNewPrivileges is set. Failing hard
// there would break every image build, so /proc stays best-effort and the
// toolchain gets an explicit GOROOT instead.
func TestProcMountIsBestEffortWhenNested(t *testing.T) {
	script := UnshareStepRunner{NestedUserns: true}.buildScript("/rootfs", []string{"go build ./..."})

	i := strings.Index(script, "mount -t proc")
	if i < 0 {
		t.Fatal("the proc mount must still be attempted when nested")
	}
	line := script[i:]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	if !strings.Contains(line, "|| true") {
		t.Fatalf("nested has no CAP_SYS_ADMIN; a strict mount breaks every build:\n%s", line)
	}
}

// Without this the build depends on /proc/self/exe, which is exactly what is
// unavailable nested. It must stay conditional on `go env GOROOT` failing:
// forcing a GOROOT from the first `go` on PATH pinned the runner toolchain
// over the image's own and every package died with "compile: version does not
// match go tool version".
func TestStepsExportAGorootFallback(t *testing.T) {
	for name, nested := range map[string]bool{"host": false, "nested": true} {
		t.Run(name, func(t *testing.T) {
			script := UnshareStepRunner{NestedUserns: nested}.buildScript("/rootfs", []string{"go build ./..."})
			if !strings.Contains(script, "GOROOT") {
				t.Fatal("a trimmed go recovers GOROOT from /proc/self/exe; it needs an explicit fallback")
			}
			if !strings.Contains(script, "go env GOROOT") {
				t.Fatal("the fallback must defer to go's own answer whenever go can give one")
			}
		})
	}
}

// Without /proc, `go env GOROOT` prints its complaint on STDOUT and exits 0,
// so redirecting stderr keeps nothing out: the fallback read the sentence
// "go: cannot find GOROOT directory: ..." and treated it as an answer, leaving
// GOROOT unset and the build dead on the very error the fallback exists to
// prevent. Only an absolute path counts as an answer.
func TestGorootFallbackIgnoresGoComplaintOnStdout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	// Mimics a toolchain with no /proc to introspect: prints the error on
	// stdout, exits 0.
	body := "#!/bin/sh\necho \"go: cannot find GOROOT directory: 'go' binary is trimmed and GOROOT is not set\"\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("go1.26.6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stub, filepath.Join(root, "bin", "go")); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command("/bin/sh", "-c",
		"export PATH="+filepath.Join(root, "bin")+":$PATH; "+
			gorootFallbackExport+"echo \"GOROOT=[$GOROOT]\"").CombinedOutput()
	if err != nil {
		t.Fatalf("fallback script failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "GOROOT=["+root+"]") {
		t.Fatalf("the fallback accepted go's error text as a GOROOT instead of deriving one:\n%s", out)
	}
}

// The runner's own env names paths under its /cache mount; a build step chroots
// into an image rootfs where that mount does not exist. `go` refuses to create
// GOTMPDIR itself and stops with "creating work dir: stat /cache/go-tmp: no
// such file or directory", so a cache var pointing outside the rootfs has to go
// — while one that does resolve must survive, or every build loses its cache.
func TestStaleCacheDirsAreUnsetButLiveOnesSurvive(t *testing.T) {
	live := t.TempDir()
	out, err := exec.Command("/bin/sh", "-c",
		"export GOTMPDIR=/nonexistent/go-tmp GOCACHE="+live+"; "+
			staleCacheDirUnset+
			`echo "GOTMPDIR=[${GOTMPDIR:-unset}] GOCACHE=[${GOCACHE:-unset}]"`).CombinedOutput()
	if err != nil {
		t.Fatalf("unset script failed: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, "GOTMPDIR=[unset]") {
		t.Fatalf("a cache dir absent from the rootfs must be dropped:\n%s", got)
	}
	if !strings.Contains(got, "GOCACHE=["+live+"]") {
		t.Fatalf("a cache dir that resolves must be kept, or the build loses its cache:\n%s", got)
	}
}

// Nested, only uid 0 is mapped: apt's drop to _apt (uid 42) fails with
// "seteuid 42 failed: Invalid argument" and the runtime stage of every image
// that installs a package dies there. The runner must tell apt to stay root —
// and only nested, since on the host a subuid range maps 42 and the drop is
// the safer default. The file must also be gone before the layer is taken.
func TestNestedRunnerKeepsAptRootAndCleansUp(t *testing.T) {
	nested := UnshareStepRunner{NestedUserns: true}.buildScript("/rootfs", []string{"apt-get update"})
	if !strings.Contains(nested, `APT::Sandbox::User "root";`) {
		t.Fatalf("nested runner must stop apt from dropping to an unmapped uid:\n%s", nested)
	}
	write := strings.Index(nested, `APT::Sandbox::User`)
	run := strings.Index(nested, "apt-get update")
	rm := strings.Index(nested, "rm -f '/rootfs'"+nestedAptConfPath)
	if write < 0 || run < 0 || rm < 0 || !(write < run && run < rm) {
		t.Fatalf("the apt config must be written before the steps and removed after them (write=%d run=%d rm=%d)", write, run, rm)
	}

	host := UnshareStepRunner{}.buildScript("/rootfs", []string{"apt-get update"})
	if strings.Contains(host, "APT::Sandbox::User") {
		t.Fatal("on the host apt must keep dropping privileges: the subuid range maps _apt")
	}
}

// Nested, mount(2) is refused, so bind-mounting the resolver over the stage
// silently left the empty file `touch` had created: musl fell back to
// 127.0.0.1 and apk reported "DNS: transient error" on every Alpine stage. The
// nested runner must copy the file in — and take it out again before the
// layer is taken, restoring whatever the image shipped.
func TestNestedRunnerCopiesResolvConfAndRestoresIt(t *testing.T) {
	nested := UnshareStepRunner{NestedUserns: true}.buildScript("/rootfs", []string{"apk add git"})
	if strings.Contains(nested, "mount --bind /etc/resolv.conf") {
		t.Fatal("nested, the bind cannot succeed and hides an empty resolv.conf behind it")
	}
	cp := strings.Index(nested, "cp /etc/resolv.conf '/rootfs'/etc/resolv.conf")
	run := strings.Index(nested, "apk add git")
	restore := strings.Index(nested, "mv -f '/rootfs'/etc/resolv.conf.sklp-orig '/rootfs'/etc/resolv.conf")
	if cp < 0 || run < 0 || restore < 0 || !(cp < run && run < restore) {
		t.Fatalf("resolv.conf must be copied before the steps and restored after them (cp=%d run=%d restore=%d)", cp, run, restore)
	}

	host := UnshareStepRunner{}.buildScript("/rootfs", []string{"apk add git"})
	if !strings.Contains(host, "mount --bind /etc/resolv.conf") {
		t.Fatal("on the host the bind works and keeps the resolver out of the produced layer")
	}
}
