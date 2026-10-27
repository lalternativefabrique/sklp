package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// mux_systemd.go manages the systemd --user unit for the host gateway mux (the
// ingress-per-space front door). The mux is a HOST singleton — one per host,
// shared by every per-space-mode space — so it gets ONE fixed unit, not a
// per-space one. A per-space-mode deploy ensures it (EnsureMuxUnit), mirroring
// how the legacy singleton was brought up by the controller.

// muxUnitName is the fixed unit name of the host gateway mux.
const muxUnitName = "sklp-gateway-mux.service"

// MuxUnitName returns the host gateway mux unit name.
func MuxUnitName() string { return muxUnitName }

// MuxUnitPath is the mux unit file path under the user systemd config dir.
func MuxUnitPath() string {
	return filepath.Join(configRoot(), "systemd", "user", muxUnitName)
}

// renderMuxUnit builds the .service content for the mux. selfExe is the absolute
// path of the running sklp binary. listen is the host front-door address. PURE.
func renderMuxUnit(selfExe, listen string) string {
	return "[Unit]\n" +
		"Description=sklp gateway mux (ingress-per-space front door)\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n\n" +
		"[Service]\n" +
		"Type=simple\n" +
		// The mux binds both the HTTP (-listen) and HTTPS/SNI (default :9443)
		// front doors; the unit keeps the TLS default so a host gets HTTPS
		// passthrough without extra config.
		fmt.Sprintf("ExecStart=%s gateway mux -listen %s\n", selfExe, listen) +
		// The mux is the front door of EVERY space on the host and holds two
		// descriptors per connection in flight, so the usual 1024 default caps
		// it around 500 concurrent connections — reachable by ordinary traffic
		// across a handful of busy sites, not just by an attack. 65536 is what
		// nginx/HAProxy/traefik units set for the same reason.
		fmt.Sprintf("LimitNOFILE=%d\n", systemdNOFILE) +
		"Restart=on-failure\n" +
		"RestartSec=2\n\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

// EnsureMuxUnit writes (idempotently) the host mux unit and enables+starts it.
// selfExe must be the absolute current binary path. The unit is rewritten every
// call so a binary move or listen change is picked up; enable --now is
// idempotent, so a second per-space deploy reuses the already-running mux.
func EnsureMuxUnit(selfExe, listen string) error {
	path := MuxUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create user systemd dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderMuxUnit(selfExe, listen)), 0o644); err != nil {
		return fmt.Errorf("write mux unit %s: %w", path, err)
	}
	if err := runCommand("systemctl", daemonReloadArgs()...); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	if err := runCommand("systemctl", "--user", "enable", "--now", muxUnitName); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w", muxUnitName, err)
	}
	return nil
}

// StopMuxUnit stops + disables the mux unit and removes its file. Best-effort
// and idempotent: a missing unit is not an error.
func StopMuxUnit() error {
	_ = runCommand("systemctl", "--user", "disable", "--now", muxUnitName)
	_ = os.Remove(MuxUnitPath())
	_ = runCommand("systemctl", daemonReloadArgs()...)
	return nil
}

// MuxUnitActive reports whether the host mux unit is active (running). Used by
// `gateway status`. A non-systemd host (or absent unit) reads as not active.
func MuxUnitActive() bool {
	out, err := runCommandOutput("systemctl", "--user", "is-active", muxUnitName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "active"
}

// MuxListenAddr is the address the host mux front door binds, overridable with
// SKLP_GATEWAY_LISTEN. Shared by the deploy path and by `sklet agent`, so a
// node and a deploy agree on which port the front door owns.
func MuxListenAddr() string {
	if a := os.Getenv("SKLP_GATEWAY_LISTEN"); a != "" {
		return a
	}
	return ":9080"
}

// systemdNOFILE is the open-file limit given to the long-lived network units
// sklp installs. It is a ceiling, not an allocation: the kernel hands out
// descriptors on demand, so raising it costs nothing until they are used.
const systemdNOFILE = 65536
