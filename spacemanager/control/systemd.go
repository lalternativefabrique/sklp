package control

import (
	"fmt"
	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemd --user integration — Podman-style: ONE transient-free user unit per
// space (sklp-space-<X>.service) runs the detached supervisor. systemd owns
// restart-on-failure and (with linger) boot-time start, so a deployed space
// survives the terminal closing AND a reboot. Everything here is best-effort and
// idempotent; a host without a user session degrades to foreground (see the CLI).
//
// This file is gated on embedded_runtime to keep the control package's tag-free
// surface (protocol.go/client.go) untouched. The exec and LookPath seams are
// injectable so unit tests assert the generated unit + argv WITHOUT spawning
// systemctl.

// unitPrefix names every per-space user unit.
const unitPrefix = "sklp-space-"

// runCommand / lookPath are the exec seams (overridable in tests). Production
// wires them to os/exec; tests record argv and stub availability.
var (
	runCommand = func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	runCommandOutput = func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	}
	lookPath = exec.LookPath
)

// UnitName is the systemd --user unit name for a space.
func UnitName(space string) string { return unitPrefix + space + ".service" }

// ExitConfigError is the exit status the supervisor uses for a failure no
// restart can fix. The unit carries it as RestartPreventExitStatus, so the two
// must stay in sync — hence one constant, read by both.
//
// 78 is EX_CONFIG from sysexits(3), which says exactly this.
const ExitConfigError = 78

// UnitPath is the unit file path under the user systemd config dir
// (~/.config/systemd/user/<unit>). Honours $XDG_CONFIG_HOME via configRoot.
func UnitPath(space string) string {
	return filepath.Join(configRoot(), "systemd", "user", UnitName(space))
}

// renderUnit builds the .service file content for a space. selfExe is the
// absolute path of the running sklp binary; the supervisor reads the manifest
// from its deterministic path, so no --manifest is needed in ExecStart. PURE.
func renderUnit(space, selfExe string, envFiles []string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=sklp deploy space %s\n", space))
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	// EnvironmentFile feeds the detached supervisor the same dotenv the CLI
	// loaded, so a late-bound registry password (${S3_SECRET_KEY}, re-resolved
	// at boot — see RegistryAuthMeta) is present without the secret ever being
	// written into the unit. "-" prefix: a missing file is not a unit failure.
	for _, f := range envFiles {
		b.WriteString(fmt.Sprintf("EnvironmentFile=-%s\n", f))
	}
	// The secrets a REMOTE apply resolved. Listed last so an operator's dotenv
	// stays authoritative on a node that has both, and "-" prefixed because a
	// space deployed locally has none.
	b.WriteString(fmt.Sprintf("EnvironmentFile=-%s\n", SpaceEnvPath(space)))
	// The data root is a property of the NODE, not of a space: a supervisor unit
	// started from a shell that has it would otherwise lose it at the next boot,
	// and refuse to run a space with volumes on a node that stores them fine.
	if dir := xdg.DataRoot(); dir != "" {
		b.WriteString(fmt.Sprintf("Environment=%s=%s\n", xdg.DataRootEnv, dir))
	}
	for _, env := range []string{spacemanager.TrustedRegistriesEnv, spacemanager.RequireImageDigestEnv} {
		if v := os.Getenv(env); v != "" {
			b.WriteString(fmt.Sprintf("Environment=%s=%s\n", env, v))
		}
	}
	b.WriteString(fmt.Sprintf("ExecStart=%s %s--space %s\n", selfExe, superviseVerb(selfExe), space))
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2\n")
	// A configuration error is not transient: the manifest asks for something
	// this node cannot provide, and relaunching every RestartSec only hides it
	// behind an "activating" unit that never settles. Exiting on this code puts
	// the service in `failed`, where the reason is one `systemctl status` away.
	b.WriteString(fmt.Sprintf("RestartPreventExitStatus=%d\n\n", ExitConfigError))
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// superviseVerb is the argv that reaches the hidden supervisor daemon, which
// differs by binary: sklp keeps it under `deploy`, while sklet — being the node
// agent itself — registers it at its root and rejects `deploy` outright. Getting
// this wrong is invisible until boot, where systemd crashloops the unit on
// "unknown verb". Returns a trailing space so the caller can concatenate.
func superviseVerb(selfExe string) string {
	if filepath.Base(selfExe) == "sklet" {
		return "__supervise "
	}
	return "deploy __supervise "
}

// systemdAvailable reports whether `systemctl --user` is usable here: the binary
// exists AND a user session bus answers a trivial query. It is FALSE in CI, a
// non-login ssh, or a container without a user systemd (no $XDG_RUNTIME_DIR /
// $DBUS bus) — the caller then degrades to foreground instead of failing.
func systemdAvailable() bool {
	if _, err := lookPath("systemctl"); err != nil {
		return false
	}
	// `is-system-running` returns non-zero in degraded/maintenance states, so we
	// only require that the command CONNECTS to the user bus (no error talking to
	// it). `show` is the cheapest query that fails cleanly without a session.
	if _, err := runCommandOutput("systemctl", "--user", "show", "--property=Version"); err != nil {
		return false
	}
	return true
}

// SystemdAvailable is the exported probe the CLI calls to choose detached
// (systemd) vs foreground. See systemdAvailable.
func SystemdAvailable() bool { return systemdAvailable() }

// EnsureUnit writes (idempotently) the space's user unit, reloads the user
// daemon, and enables+starts it. selfExe must be the absolute path of the
// current binary (os.Executable). The unit is rewritten every call so a binary
// move or arg change is picked up; enable --now is idempotent.
func EnsureUnit(space, selfExe string, envFiles ...string) error {
	path := UnitPath(space)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create user systemd dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderUnit(space, selfExe, envFiles)), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", path, err)
	}
	if err := runCommand("systemctl", daemonReloadArgs()...); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	if err := runCommand("systemctl", enableNowArgs(space)...); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w", UnitName(space), err)
	}
	return nil
}

// EnableLinger enables user lingering so the space's units start at boot and
// keep running with no active session. BEST-EFFORT: enabling linger may require
// polkit/root, so a failure returns an error the caller WARNS on (the space
// won't survive a reboot) but must NOT fail the deploy on. No-op if already on.
func EnableLinger() error {
	user := os.Getenv("USER")
	if user == "" {
		if u := os.Getenv("LOGNAME"); u != "" {
			user = u
		}
	}
	if user == "" {
		return fmt.Errorf("cannot determine current user for linger")
	}
	// Already lingering? loginctl reports Linger=yes; skip the privileged call.
	if out, err := runCommandOutput("loginctl", "show-user", user, "--property=Linger"); err == nil {
		if strings.Contains(out, "Linger=yes") {
			return nil
		}
	}
	if err := runCommand("loginctl", "enable-linger", user); err != nil {
		return fmt.Errorf("loginctl enable-linger %s: %w", user, err)
	}
	return nil
}

// StopUnit stops + disables the space unit, removes the unit file, and reloads
// the user daemon. Best-effort and idempotent: a missing unit is not an error.
func StopUnit(space string) error {
	_ = runCommand("systemctl", disableNowArgs(space)...)
	_ = os.Remove(UnitPath(space))
	_ = runCommand("systemctl", daemonReloadArgs()...)
	return nil
}

// DisableUnit is an alias of StopUnit (disable --now already stops). Kept as a
// distinct name for the J5/down call sites that read better as "disable".
func DisableUnit(space string) error { return StopUnit(space) }

// --- argv builders (pure, unit-tested) -------------------------------------

func daemonReloadArgs() []string { return []string{"--user", "daemon-reload"} }

func enableNowArgs(space string) []string {
	return []string{"--user", "enable", "--now", UnitName(space)}
}

func disableNowArgs(space string) []string {
	return []string{"--user", "disable", "--now", UnitName(space)}
}
