package control

// systemd_runapi.go installs `sklp run --listen` as a systemd --user service.
//
// A CI node is shelved and unshelved twice a day by its scheduler, so an API
// started by hand is gone after the first cycle. Installing it as a unit is
// what makes the node survive that — and a crash, and a reboot.
//
// It reuses the space units' machinery (daemon-reload, enable --now, linger)
// but renders its own unit: a run API serves pipelines and has no space to
// supervise, so `deploy __supervise --space` does not apply to it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RunAPIUnitName is the systemd --user unit for the node run API. Fixed, not
// per-space: a node exposes one API regardless of what it runs.
const RunAPIUnitName = "sklp-run-api.service"

// RunAPIUnitPath is the unit file path under the user systemd config dir.
func RunAPIUnitPath() string {
	return filepath.Join(configRoot(), "systemd", "user", RunAPIUnitName)
}

// renderRunAPIUnit builds the .service content. selfExe is the absolute path of
// the running sklp binary; advertise and workRoot may be empty, in which case
// their flags are omitted rather than passed blank. concurrency <= 1 omits
// --concurrency, keeping the historical one-run-at-a-time behaviour. PURE.
// RunAPIOptions are the `sklp run --listen` settings, persisted into the unit's
// ExecStart by --install.
type RunAPIOptions struct {
	Addr        string
	Advertise   string
	WorkRoot    string
	Concurrency int
	// AllowHostSteps lets a `space: false` pipeline run on the node itself. The
	// pipeline YAML comes from the branch being tested, so without it a PR
	// could opt out of the sandbox.
	AllowHostSteps bool
}

func renderRunAPIUnit(selfExe string, o RunAPIOptions) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=sklp run API (CI node)\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString(fmt.Sprintf("ExecStart=%s run --listen %s", selfExe, o.Addr))
	if o.Advertise != "" {
		b.WriteString(" --advertise " + o.Advertise)
	}
	if o.WorkRoot != "" {
		b.WriteString(" --work-root " + o.WorkRoot)
	}
	if o.Concurrency > 1 {
		b.WriteString(fmt.Sprintf(" --concurrency %d", o.Concurrency))
	}
	if o.AllowHostSteps {
		b.WriteString(" --allow-host-steps")
	}
	b.WriteString("\n")
	// always, not on-failure: the API exiting cleanly still leaves the node
	// unable to accept work, which is the failure that matters here.
	b.WriteString(fmt.Sprintf("LimitNOFILE=%d\n", systemdNOFILE))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// EnsureRunAPIUnit writes the unit, reloads the user daemon and enables+starts
// it. Idempotent: the unit is rewritten every call so a moved binary or changed
// address is picked up, and enable --now can be repeated.
func EnsureRunAPIUnit(selfExe string, o RunAPIOptions) error {
	path := RunAPIUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create user systemd dir: %w", err)
	}
	unit := renderRunAPIUnit(selfExe, o)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", path, err)
	}
	if err := runCommand("systemctl", daemonReloadArgs()...); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	if err := runCommand("systemctl", "--user", "enable", "--now", RunAPIUnitName); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w", RunAPIUnitName, err)
	}
	return nil
}
