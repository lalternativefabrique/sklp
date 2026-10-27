//go:build !linux

// Package limarelay re-runs the current sklp invocation inside a Linux VM,
// giving non-Linux hosts the embedded runc runtime. It provisions the VM on
// first use, keeps the guest sklp (and skt) binaries in sync with host-side
// linux builds, then runs the invocation there so stdio, signals and the exit
// code all belong to the guest run.
//
// Three VM providers are supported, auto-detected in this order and forced
// with SKLP_VM_PROVIDER=vz|lima|orbstack:
//
//   - vz — sklp's own VM on Virtualization.framework (darwin/arm64, cgo
//     builds). Where it exists it is THE backend, needing nothing installed
//     on the host, with the same debian:trixie-slim guest the runner rootfs
//     is built from. Missing artifacts are an error naming `sklp vz build`,
//     never a silent fall-through to another VM.
//   - lima — the validated path end to end: a full `sklp dev` stack with
//     images, runner rootfs builds, service DNS and host ingress.
//   - orbstack — relay, provisioning and single-shot verbs work, but a space
//     gets no egress route: its containers reach the bridge and each other,
//     while anything off-machine fails with ENETUNREACH, so a service that
//     fetches dependencies at start cannot come up. Keep it for read-only
//     verbs until that is fixed.
//
// The first two hand the run over with syscall.Exec; vz cannot, because its
// VM lives inside this process — it implements relayer instead.
package limarelay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

const vmName = "sklp"

// guestSetupMarker records that a guest has been provisioned (subuid ranges,
// tun, sysctls). Image-based providers bake that in and report it present.
const guestSetupMarker = "/etc/sklp-host-setup-done-v2"

// provider is the tiny VM surface the relay needs: status, provision, a way
// to run root and user commands in the guest, host→guest path mapping, and
// the final argv that replays sklp inside the guest.
type provider interface {
	label() string
	status() string // "Running", other (stopped…), or "" when absent
	create() error
	start() error
	guestPath(host string) string
	rootExec(script string) error
	userOut(script string) string
	install(hostBin, name string) error
	execArgv(workdir string, tail []string) (bin string, argv []string)
}

// GuestExitError carries a guest exit status back to main, which turns it
// into this process's own status so a relayed run is indistinguishable from a
// local one.
type GuestExitError struct{ Code int }

func (e *GuestExitError) Error() string {
	return fmt.Sprintf("guest exited with status %d", e.Code)
}

// imageProvider marks providers whose guest tooling is baked into the rootfs
// image rather than installed on first use.
type imageProvider interface{ toolsBaked() }

// relayer is implemented by providers that own the guest process themselves
// instead of handing it over with syscall.Exec. A VM built on
// Virtualization.framework lives inside this process, so exec'ing away would
// tear it down; such a provider drives the run and reports its exit status.
type relayer interface {
	relay(workdir string, tail []string) error
}

// argvRelayer runs an explicit argv in the guest — argv[0] is a guest path,
// not the baked `sklp`. This is how a host-side orchestration executes a
// freshly cross-compiled binary from the shared home instead of whatever
// version the guest image happens to carry.
type argvRelayer interface {
	relayArgv(workdir string, argv []string) error
}

// RelayCommand runs argv verbatim inside the VM with workdir as the guest
// working directory. Unlike RelaySelf it does not replay this process's
// arguments: the caller names the binary and argv exactly.
func RelayCommand(workdir string, argv []string) error {
	p, err := pickProvider()
	if err != nil {
		return err
	}
	if err := ensureVM(p); err != nil {
		return err
	}
	if err := ensureGuestTools(p); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if !workdirMounted(workdir, home) {
		return fmt.Errorf("%s is outside your home directory — the VM only mounts %s", workdir, home)
	}
	r, ok := p.(argvRelayer)
	if !ok {
		return fmt.Errorf("the %s provider cannot relay an explicit command", p.label())
	}
	fmt.Fprintf(os.Stderr, "[sklp] relaying into %s VM %q\n", p.label(), vmName)
	return r.relayArgv(workdir, argv)
}

// RelaySelf replays this process's exact argv as `sklp …` inside the VM, with
// the guest working directory at workdir. It only returns on failure.
func RelaySelf(workdir string) error {
	p, err := pickProvider()
	if err != nil {
		return err
	}
	if err := ensureVM(p); err != nil {
		return err
	}
	if err := ensureGuestTools(p); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if !workdirMounted(workdir, home) {
		return fmt.Errorf("%s is outside your home directory — the VM only mounts %s", workdir, home)
	}

	fmt.Fprintf(os.Stderr, "[sklp] relaying into %s VM %q\n", p.label(), vmName)
	if r, ok := p.(relayer); ok {
		return r.relay(workdir, os.Args[1:])
	}
	bin, argv := p.execArgv(workdir, os.Args[1:])
	return syscall.Exec(bin, argv, os.Environ())
}

// forwardedEnv are the variables a relayed run needs from the host session.
// A provider shell (limactl shell, orbctl run) starts a fresh login
// environment, so nothing crosses on its own: without this, a pipeline that
// resolves stack-config secrets fails inside the VM with "passphrase is
// empty" even though the operator did export it.
//
// The list is explicit rather than a wholesale copy of os.Environ(): the
// guest is a different OS with its own PATH, HOME and toolchain, and a
// blanket forward would clobber them.
var forwardedEnv = []string{
	"SKLP_SECRETS_PASSPHRASE",
	"SKLP_SECRETS_PASSPHRASE_NEW",
	"GITHUB_TOKEN",
	"SKLP_DIST_BASE_URL",
	"SKLP_VERSION",
}

// guestCommand renders `env K=V … sklp <args>` so the forwarded variables
// reach the relayed process. Values travel as argv, which is readable via
// /proc on the guest — acceptable for a single-user dev VM, and the same
// exposure `env VAR=x cmd` already has on the host.
func guestCommand(tail []string) []string {
	var cmd []string
	for _, k := range forwardedEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd = append(cmd, k+"="+v)
			continue
		}
		// The vault lives on the host: the guest has no keychain, no
		// `security`, and none of the host's sklp config. So a passphrase the
		// operator never exported has to be resolved HERE and carried across,
		// or the relayed run fails with "passphrase is empty" while the host
		// could read it all along.
		if k == stackconfig.PassphraseEnv {
			if v, _, err := stackconfig.ResolvePassphrase("."); err == nil && v != "" {
				cmd = append(cmd, k+"="+v)
			}
		}
	}
	if len(cmd) > 0 {
		cmd = append([]string{"env"}, cmd...)
	}
	cmd = append(cmd, "sklp")
	return append(cmd, tail...)
}

// workdirMounted reports whether dir is visible in the guest: the VM mounts
// the host home, so the home itself and anything under it qualifies.
func workdirMounted(dir, home string) bool {
	return dir == home || strings.HasPrefix(dir, home+string(filepath.Separator))
}

// VMRunning reports whether guest-side state (dev statefiles, spaces) is
// reachable, so a host-side command knows to relay instead of acting locally.
//
// For a manager-backed VM that means an instance is up. A provider that boots
// its VM per command has no instance to report, yet its guest state is just as
// reachable — it lives on the rootfs image and comes back with the next boot —
// so being able to relay at all is what counts.
func VMRunning() bool {
	p, err := pickProvider()
	if err != nil {
		return false
	}
	if _, perRun := p.(relayer); perRun {
		return true
	}
	return p.status() == "Running"
}

// newNativeProvider is set on platforms that ship an in-process VM backend
// (darwin/arm64 builds it on Virtualization.framework). It stays nil
// elsewhere, so those hosts see only the shell-based providers.
var newNativeProvider func() (provider, error)

func pickProvider() (provider, error) {
	switch forced := os.Getenv("SKLP_VM_PROVIDER"); forced {
	case "vz":
		if newNativeProvider == nil {
			return nil, fmt.Errorf("SKLP_VM_PROVIDER=vz is only available on darwin/arm64")
		}
		return newNativeProvider()
	case "lima":
		if p, err := exec.LookPath("limactl"); err == nil {
			return limaProvider{limactl: p}, nil
		}
		return nil, fmt.Errorf("SKLP_VM_PROVIDER=lima but limactl is not installed (brew install lima)")
	case "orbstack":
		if p, err := exec.LookPath("orbctl"); err == nil {
			return orbProvider{orbctl: p}, nil
		}
		return nil, fmt.Errorf("SKLP_VM_PROVIDER=orbstack but orbctl is not installed (https://orbstack.dev)")
	case "":
	default:
		return nil, fmt.Errorf("unknown SKLP_VM_PROVIDER %q (vz | lima | orbstack)", forced)
	}
	// Where the native backend exists it IS the default: it needs nothing
	// installed, and quietly relaying through a different VM would hide which
	// Linux a run actually happened on. Unbuilt artifacts are reported with the
	// command that fixes them rather than worked around — lima and orbstack
	// stay reachable, but only when asked for by name.
	if newNativeProvider != nil {
		p, err := newNativeProvider()
		if err == nil {
			return p, nil
		}
		// Falling through is announced rather than silent: which Linux a run
		// happened on is the kind of thing that must not be guessed at when a
		// result later looks wrong.
		fmt.Fprintf(os.Stderr, "[sklp] vz: %v — falling back (run `sklp vz build` to use the native VM)\n", err)
	}
	if p, err := exec.LookPath("limactl"); err == nil {
		return limaProvider{limactl: p}, nil
	}
	if p, err := exec.LookPath("orbctl"); err == nil {
		return orbProvider{orbctl: p}, nil
	}
	return nil, fmt.Errorf("this host cannot run rootless runc; install Lima (brew install lima) or OrbStack to relay into a Linux VM")
}

func ensureVM(p provider) error {
	switch p.status() {
	case "Running":
	case "":
		fmt.Fprintf(os.Stderr, "[sklp] %s: preparing Linux VM %q (first run may take a few minutes)\n", p.label(), vmName)
		if err := p.create(); err != nil {
			return fmt.Errorf("%s: create VM: %w", p.label(), err)
		}
	default:
		fmt.Fprintf(os.Stderr, "[sklp] %s: starting VM %q\n", p.label(), vmName)
		if err := p.start(); err != nil {
			return fmt.Errorf("%s: start VM: %w", p.label(), err)
		}
	}
	return ensureGuestSetup(p)
}

// ensureGuestSetup applies the rootless-runc prerequisites once per VM:
// uidmap for newuidmap/newgidmap, a subuid/subgid grant at the runtime's
// 100000 base for the guest user (providers grant none, or one at another
// base, breaking the runner-build newuidmap range map), the Ubuntu 24.04
// AppArmor sysctl that otherwise denies unprivileged user namespaces, and
// the tun module for slirp4netns.
func ensureGuestSetup(p provider) error {
	const marker = guestSetupMarker
	if p.userOut("test -f "+marker+" && echo ok") == "ok" {
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[sklp] %s: preparing the VM for rootless runc\n", p.label())
	script := `set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -q && apt-get install -yq uidmap
u=` + u.Username + `
grep -q "^$u:100000:" /etc/subuid || printf '%s:100000:65536\n' "$u" >>/etc/subuid
grep -q "^$u:100000:" /etc/subgid || printf '%s:100000:65536\n' "$u" >>/etc/subgid
printf 'kernel.apparmor_restrict_unprivileged_userns=0\n' >/etc/sysctl.d/99-sklp.conf
sysctl --system >/dev/null || true
modprobe tun || true
printf 'tun\n' >/etc/modules-load.d/sklp-tun.conf
touch ` + marker
	if err := p.rootExec(script); err != nil {
		return fmt.Errorf("%s: guest setup: %w", p.label(), err)
	}
	return nil
}

// ensureGuestTools installs (or refreshes) the linux sklp build inside the
// VM, and skt alongside it when a host-side linux skt exists (run: services
// resolve their toolchain through it). The host binaries are the wrong OS for
// the guest, so linux builds must exist on the host; each is found via its
// env override, next to this executable, or under the sklp cache — sklp also
// falls back to the published dist build — and is (re)installed only when its
// hash differs from the guest's.
func ensureGuestTools(p provider) error {
	bin, err := findLinuxTool("sklp", "SKLP_LIMA_BIN")
	if err != nil {
		bin, err = downloadLinuxSklp()
	}
	if err != nil {
		return fmt.Errorf("no linux sklp build for the VM — build one:\n  GOOS=linux GOARCH=%s go build -o ~/.cache/sklp/lima/sklp-linux-%s ./cmd/sklp\n(%v)",
			runtime.GOARCH, runtime.GOARCH, err)
	}
	if err := syncGuestTool(p, "sklp", bin); err != nil {
		return err
	}
	if skt, err := findLinuxTool("skt", "SKT_LIMA_BIN"); err == nil {
		if err := syncGuestTool(p, "skt", skt); err != nil {
			return err
		}
	}
	return nil
}

func syncGuestTool(p provider, name, bin string) error {
	// An image-based provider ships the tools inside its rootfs; there is
	// nothing to push and nothing to report.
	if _, baked := p.(imageProvider); baked {
		return nil
	}
	hostSum, err := fileSHA256(bin)
	if err != nil {
		return err
	}
	guestSum := p.userOut("sha256sum /usr/local/bin/" + name + " 2>/dev/null | cut -d' ' -f1")
	if guestSum == hostSum {
		return nil
	}
	fmt.Fprintf(os.Stderr, "[sklp] %s: installing %s (linux/%s) into the VM\n", p.label(), name, runtime.GOARCH)
	if err := p.install(bin, name); err != nil {
		return fmt.Errorf("%s: install %s in VM: %w", p.label(), name, err)
	}
	return nil
}

// --- Lima -------------------------------------------------------------------

type limaProvider struct{ limactl string }

func (l limaProvider) label() string { return "lima" }

func (l limaProvider) status() string {
	out, err := exec.Command(l.limactl, "list", vmName, "--format", "{{.Status}}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (l limaProvider) create() error {
	return runLoud(l.limactl, "start", "--name="+vmName, "--tty=false", "--mount-writable", "template://default")
}

func (l limaProvider) start() error {
	return runLoud(l.limactl, "start", "--tty=false", vmName)
}

// Lima mounts the host home at the same path in the guest.
func (l limaProvider) guestPath(host string) string { return host }

func (l limaProvider) rootExec(script string) error {
	return runLoud(l.limactl, "shell", vmName, "--", "sudo", "sh", "-c", script)
}

func (l limaProvider) userOut(script string) string {
	out, err := exec.Command(l.limactl, "shell", vmName, "--", "sh", "-c", script).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The home mount is read-visible in the guest but /usr/local/bin is not, and
// limactl cp handles the sparse-file quirks of virtiofs, so copy then install.
func (l limaProvider) install(hostBin, name string) error {
	if err := runLoud(l.limactl, "cp", hostBin, vmName+":/tmp/"+name+"-new"); err != nil {
		return err
	}
	return runLoud(l.limactl, "shell", vmName, "--", "sudo", "sh", "-c",
		"install -m 0755 /tmp/"+name+"-new /usr/local/bin/"+name+" && rm -f /tmp/"+name+"-new")
}

func (l limaProvider) execArgv(workdir string, tail []string) (string, []string) {
	argv := []string{"limactl", "shell", "--workdir", l.guestPath(workdir), vmName, "--"}
	argv = append(argv, guestCommand(tail)...)
	return l.limactl, argv
}

// --- OrbStack ---------------------------------------------------------------

type orbProvider struct{ orbctl string }

func (o orbProvider) label() string { return "orbstack" }

func (o orbProvider) status() string {
	out, err := exec.Command(o.orbctl, "info", vmName, "-f", "json").Output()
	if err != nil {
		return ""
	}
	var info struct {
		Record struct {
			State string `json:"state"`
		} `json:"record"`
	}
	if json.Unmarshal(out, &info) != nil {
		return ""
	}
	if strings.EqualFold(info.Record.State, "running") {
		return "Running"
	}
	return info.Record.State
}

func (o orbProvider) create() error {
	return runLoud(o.orbctl, "create", "ubuntu", vmName)
}

func (o orbProvider) start() error {
	return runLoud(o.orbctl, "start", vmName)
}

// OrbStack shares the mac filesystem under /mnt/mac in every machine.
func (o orbProvider) guestPath(host string) string { return "/mnt/mac" + host }

func (o orbProvider) rootExec(script string) error {
	return runLoud(o.orbctl, "run", "-m", vmName, "-u", "root", "sh", "-c", script)
}

func (o orbProvider) userOut(script string) string {
	out, err := exec.Command(o.orbctl, "run", "-m", vmName, "sh", "-c", script).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The shared /mnt/mac view makes the host binary directly readable in the
// guest: install straight from it, no copy step.
func (o orbProvider) install(hostBin, name string) error {
	return runLoud(o.orbctl, "run", "-m", vmName, "-u", "root",
		"install", "-m", "0755", o.guestPath(hostBin), "/usr/local/bin/"+name)
}

func (o orbProvider) execArgv(workdir string, tail []string) (string, []string) {
	argv := []string{"orbctl", "run", "-m", vmName, "-w", o.guestPath(workdir)}
	argv = append(argv, guestCommand(tail)...)
	return o.orbctl, argv
}

// --- shared helpers ---------------------------------------------------------

func findLinuxTool(name, envKey string) (string, error) {
	file := name + "-linux-" + runtime.GOARCH
	var candidates []string
	if p := os.Getenv(envKey); p != "" {
		candidates = append(candidates, p)
	}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), file))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".cache", "sklp", "lima", file))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("searched: %s", strings.Join(candidates, ", "))
}

// downloadLinuxSklp fetches the published linux sklp for this arch from the
// dist bucket (the same layout the curl installers use) into the sklp cache,
// so a curl-installed mac sklp needs no Go toolchain to provision the VM.
// SKLP_DIST_BASE_URL overrides the origin, SKLP_VERSION the version.
func downloadLinuxSklp() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return downloadDistBinary("sklp-linux-"+runtime.GOARCH,
		filepath.Join(home, ".cache", "sklp", "lima", "sklp-linux-"+runtime.GOARCH))
}

// FetchSktLinux returns a linux skt for arch, downloading it from the dist
// bucket into the vz cache on the first call. skt is a separate Rust crate
// this repo cannot cross-build, so the published binary is the only source a
// mac has; the vz rootfs build bakes it into the guest image.
func FetchSktLinux(arch string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(home, ".cache", "sklp", "vz", "skt-linux-"+arch)
	if st, err := os.Stat(dst); err == nil && !st.IsDir() {
		return dst, nil
	}
	got, derr := downloadDistBinary("skt-linux-"+arch, dst)
	if derr == nil {
		return got, nil
	}
	// The dist bucket only publishes skt for amd64 today (see
	// scripts/build-cli-binaries.sh: the arm64 cross-build waits for a
	// consumer — this is that consumer). Until it ships, accept a locally
	// built skt left by an earlier lima provisioning.
	local := filepath.Join(home, ".cache", "sklp", "lima", "skt-linux-"+arch)
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local, nil
	}
	return "", derr
}

// downloadDistBinary fetches one published binary from the dist bucket (the
// same layout the curl installers use) into dst. SKLP_DIST_BASE_URL overrides
// the origin, SKLP_VERSION the version.
func downloadDistBinary(name, dst string) (string, error) {
	base := os.Getenv("SKLP_DIST_BASE_URL")
	if base == "" {
		base = "https://skalpai-dist.s3.fr-par.scw.cloud/cli"
	}
	version := os.Getenv("SKLP_VERSION")
	if version == "" {
		version = "latest"
	}
	url := strings.TrimRight(base, "/") + "/" + version + "/" + name

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "[sklp] dist: downloading %s\n", url)
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func runLoud(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
