//go:build darwin && arm64 && cgo

package limarelay

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// vzProvider runs the guest in a VM built directly on Virtualization.framework
// instead of shelling out to a VM manager. The VM lives inside this process,
// so unlike lima/orbstack it cannot hand the run over with syscall.Exec — it
// implements relayer instead and drives the guest over vsock.
//
// The kernel and rootfs are content-addressed artifacts under vzStateDir; the
// kernel must carry VIRTIO_FS/VSOCKETS/TUN/NF_TABLES built in (a stock distro
// kernel ships them as modules and will not work).
type vzProvider struct {
	stateDir string
	kernel   string
	rootfs   string
}

func newVZProvider() (vzProvider, error) {
	dir := vzStateDir()
	p := vzProvider{
		stateDir: dir,
		kernel:   filepath.Join(dir, "kernel"),
		rootfs:   filepath.Join(dir, "rootfs.img"),
	}
	for _, f := range []string{p.kernel, p.rootfs} {
		if _, err := os.Stat(f); err != nil {
			return vzProvider{}, fmt.Errorf("vz backend: %s is missing", filepath.Base(f))
		}
	}
	return p, nil
}

func vzStateDir() string {
	if d := os.Getenv("SKLP_VZ_DIR"); d != "" {
		return d
	}
	// Honour XDG_CACHE_HOME like the rest of the sklp caches, so a test or a
	// sandboxed run can point the artifacts somewhere else.
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "sklp", "vz")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sklp-vz"
	}
	return filepath.Join(home, ".cache", "sklp", "vz")
}

func (v vzProvider) label() string { return "vz" }

// status reports on the shared VM daemon. "Running" once it is up, "" before
// the first command starts it — which is what makes ensureVM print its
// "preparing" line exactly once instead of on every invocation.
func (v vzProvider) status() string {
	if DaemonRunning() {
		return "Running"
	}
	return ""
}

// create and start both mean the same thing here: bring the daemon up. The
// distinction exists for VM managers that keep a stopped instance on disk;
// this backend has no such state — either a daemon owns the VM or none does.
func (v vzProvider) create() error { return startDaemon() }
func (v vzProvider) start() error  { return startDaemon() }

// virtiofs exposes the host home at the same path, as Lima does.
func (v vzProvider) guestPath(host string) string { return host }

// The rootfs image is provisioned when it is built (subuid ranges, mounts,
// baked binaries), so there is no first-boot setup left to run in the guest.
// ensureGuestSetup still calls these, so they must succeed without doing
// anything rather than boot a VM per probe.
func (v vzProvider) rootExec(script string) error { return nil }

// Reporting the setup marker as present keeps ensureGuestSetup from trying to
// provision an image that is already provisioned.
func (v vzProvider) userOut(script string) string {
	if strings.Contains(script, guestSetupMarker) {
		return "ok"
	}
	return ""
}

// The rootfs image already carries sklp and skt, baked when it was built.
func (v vzProvider) install(hostBin, name string) error { return nil }

func (v vzProvider) toolsBaked() {}

// execArgv exists to satisfy provider. The vz VM cannot be exec'd into, so
// RelaySelf must take the relayer path instead; reaching here is a bug.
func (v vzProvider) execArgv(workdir string, tail []string) (string, []string) {
	return "", nil
}

var errVZExec = errors.New("vz provider cannot be exec'd into; use relay()")

// relay boots the VM, runs `sklp <tail>` in it with the host cwd mapped, and
// reports the guest's exit status.
func (v vzProvider) relay(workdir string, tail []string) error {
	// The VM is shared and outlives this command, so the first caller starts
	// the daemon that owns it and everyone else just connects. Without that,
	// a long `sklp dev` would hold the rootfs image for its whole life and
	// lock out every other sklp on the machine.
	conn, err := v.connectSession()
	if err != nil {
		return err
	}
	defer conn.Close()

	req := vzRequest{
		Cwd:  v.guestPath(workdir),
		Env:  forwardedEnvPairs(),
		Argv: append([]string{"sklp"}, tail...),
	}
	code, err := runSession(conn, req)
	if err != nil {
		return err
	}
	if code != 0 {
		return &GuestExitError{Code: code}
	}
	return nil
}

// relayArgv runs an explicit argv in the guest (argvRelayer). Same session
// transport as relay; only the command line differs — the caller has already
// rendered it, binary path included.
func (v vzProvider) relayArgv(workdir string, argv []string) error {
	conn, err := v.connectSession()
	if err != nil {
		return err
	}
	defer conn.Close()
	req := vzRequest{
		Cwd:  v.guestPath(workdir),
		Env:  forwardedEnvPairs(),
		Argv: argv,
	}
	code, err := runSession(conn, req)
	if err != nil {
		return err
	}
	if code != 0 {
		return &GuestExitError{Code: code}
	}
	return nil
}

// connectSession returns a connection to the guest agent, starting the VM
// daemon first when nothing is serving yet.
func (v vzProvider) connectSession() (net.Conn, error) {
	if conn, err := net.Dial("unix", vzSocketPath()); err == nil {
		return conn, nil
	}
	if err := startDaemon(); err != nil {
		return nil, err
	}
	// The daemon writes its pidfile before it binds, so a dial can still lose
	// a short race with it. Retry briefly rather than fail a command that is
	// one moment away from working.
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.Dial("unix", vzSocketPath())
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the macOS VM is up but not accepting sessions on %s: %w", vzSocketPath(), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// forwardedEnvPairs renders the forwarded host variables as KEY=VALUE, the
// same set guestCommand puts in argv for the shell-based providers.
func forwardedEnvPairs() []string {
	var out []string
	for _, k := range forwardedEnv {
		if val, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+val)
			continue
		}
		// Same as guestCommand: the vault lives on the host (keychain,
		// `security`), so an unexported passphrase must be resolved here and
		// carried across, or the guest fails trying to run `security` itself.
		if k == stackconfig.PassphraseEnv {
			if v, _, err := stackconfig.ResolvePassphrase("."); err == nil && v != "" {
				out = append(out, k+"="+v)
			}
		}
	}
	return out
}

func init() {
	newNativeProvider = func() (provider, error) { return newVZProvider() }
}
