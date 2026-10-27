package control

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRenderUnit: the generated unit carries the right ExecStart (absolute
// binary + __supervise --space), restart policy, type, and install target —
// table-driven, without executing systemctl.
func TestRenderUnit(t *testing.T) {
	const selfExe = "/usr/local/bin/sklp"
	unit := renderUnit("production", selfExe, nil)

	wants := []string{
		"Type=simple",
		"ExecStart=/usr/local/bin/sklp deploy __supervise --space production",
		"Restart=on-failure",
		"RestartSec=2",
		"WantedBy=default.target",
		"Description=sklp deploy space production",
	}
	for _, w := range wants {
		if !strings.Contains(unit, w) {
			t.Errorf("unit missing %q:\n%s", w, unit)
		}
	}
	// No --manifest in ExecStart: the supervisor reads the deterministic path.
	if strings.Contains(unit, "--manifest") {
		t.Errorf("unit ExecStart must not pass --manifest:\n%s", unit)
	}
	// No env_files: the only EnvironmentFile is the space's own secret file,
	// which every space has — a remote apply persists what it resolved there.
	for _, line := range strings.Split(unit, "\n") {
		if !strings.HasPrefix(line, "EnvironmentFile=") {
			continue
		}
		if line != "EnvironmentFile=-"+SpaceEnvPath("production") {
			t.Errorf("unexpected EnvironmentFile %q with no env_files:\n%s", line, unit)
		}
	}
}

// TestRenderUnit_EnvFiles: env_files become EnvironmentFile=- lines (the "-"
// makes a missing file non-fatal) so the detached supervisor re-reads the same
// dotenv the CLI loaded (late-bound registry password survives the hand-off).
func TestRenderUnit_EnvFiles(t *testing.T) {
	unit := renderUnit("production", "/usr/local/bin/sklp", []string{"/home/u/shop/.env", "/home/u/shop/.env.local"})
	for _, w := range []string{
		"EnvironmentFile=-/home/u/shop/.env\n",
		"EnvironmentFile=-/home/u/shop/.env.local\n",
	} {
		if !strings.Contains(unit, w) {
			t.Errorf("unit missing %q:\n%s", w, unit)
		}
	}
}

// TestUnitNameAndPath: the unit name and path follow the per-space convention
// under the user systemd config dir.
func TestUnitNameAndPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/home/u/.config")
	if got := UnitName("pr-12"); got != "sklp-space-pr-12.service" {
		t.Fatalf("UnitName = %q", got)
	}
	if got := UnitPath("pr-12"); got != "/home/u/.config/systemd/user/sklp-space-pr-12.service" {
		t.Fatalf("UnitPath = %q", got)
	}
}

// TestSystemctlArgv: the argv the integration uses are exactly what we expect
// (no surprise flags, space named in enable/disable).
func TestSystemctlArgv(t *testing.T) {
	if got := daemonReloadArgs(); !reflect.DeepEqual(got, []string{"--user", "daemon-reload"}) {
		t.Errorf("daemonReloadArgs = %v", got)
	}
	if got := enableNowArgs("production"); !reflect.DeepEqual(got, []string{"--user", "enable", "--now", "sklp-space-production.service"}) {
		t.Errorf("enableNowArgs = %v", got)
	}
	if got := disableNowArgs("production"); !reflect.DeepEqual(got, []string{"--user", "disable", "--now", "sklp-space-production.service"}) {
		t.Errorf("disableNowArgs = %v", got)
	}
}

// TestSystemdAvailable_NoBinary: when systemctl is not on PATH, availability is
// false (the CLI then degrades to foreground).
func TestSystemdAvailable_NoBinary(t *testing.T) {
	restore := lookPath
	defer func() { lookPath = restore }()
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	if systemdAvailable() {
		t.Fatal("systemdAvailable=true with no systemctl on PATH; want false")
	}
}

// TestSystemdAvailable_NoUserBus: systemctl exists but the user bus query fails
// (no session) -> false.
func TestSystemdAvailable_NoUserBus(t *testing.T) {
	restoreLP, restoreRun := lookPath, runCommandOutput
	defer func() { lookPath, runCommandOutput = restoreLP, restoreRun }()
	lookPath = func(string) (string, error) { return "/usr/bin/systemctl", nil }
	runCommandOutput = func(string, ...string) (string, error) {
		return "Failed to connect to bus", fmt.Errorf("exit status 1")
	}

	if systemdAvailable() {
		t.Fatal("systemdAvailable=true with no user bus; want false")
	}
}

// TestSystemdAvailable_OK: systemctl exists and the user bus answers -> true.
func TestSystemdAvailable_OK(t *testing.T) {
	restoreLP, restoreRun := lookPath, runCommandOutput
	defer func() { lookPath, runCommandOutput = restoreLP, restoreRun }()
	lookPath = func(string) (string, error) { return "/usr/bin/systemctl", nil }
	runCommandOutput = func(string, ...string) (string, error) { return "Version=255\n", nil }

	if !systemdAvailable() {
		t.Fatal("systemdAvailable=false with a live user bus; want true")
	}
}

// TestEnsureUnit_WritesAndInvokes: EnsureUnit writes the unit file and runs
// daemon-reload + enable --now, with the expected argv, without a real systemctl.
func TestEnsureUnit_WritesAndInvokes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	restore := runCommand
	defer func() { runCommand = restore }()

	var calls [][]string
	runCommand = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	if err := EnsureUnit("production", "/opt/sklp"); err != nil {
		t.Fatalf("EnsureUnit: %v", err)
	}
	// Unit file written with the right ExecStart.
	raw, err := os.ReadFile(UnitPath("production"))
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	data := string(raw)
	if !strings.Contains(data, "ExecStart=/opt/sklp deploy __supervise --space production") {
		t.Fatalf("unit ExecStart wrong:\n%s", data)
	}
	if len(calls) != 2 {
		t.Fatalf("want 2 systemctl calls (reload, enable), got %v", calls)
	}
	if !reflect.DeepEqual(calls[0], []string{"systemctl", "--user", "daemon-reload"}) {
		t.Errorf("call[0] = %v", calls[0])
	}
	if !reflect.DeepEqual(calls[1], []string{"systemctl", "--user", "enable", "--now", "sklp-space-production.service"}) {
		t.Errorf("call[1] = %v", calls[1])
	}
}

// TestDisableUnit_StopsDisablesRemovesAtBoot: a down's systemd step must
// disable --now (stop + disable so Restart=on-failure does NOT relaunch) AND
// remove the unit file (so it does not come back at the next boot). Without a
// real systemctl: assert the argv + that the unit file is gone afterwards.
func TestDisableUnit_StopsDisablesRemovesAtBoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	restore := runCommand
	defer func() { runCommand = restore }()

	var calls [][]string
	runCommand = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	// Pre-create a unit file so we can assert its removal.
	if err := os.MkdirAll(filepath.Dir(UnitPath("production")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(UnitPath("production"), []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := DisableUnit("production"); err != nil {
		t.Fatalf("DisableUnit: %v", err)
	}

	// First call must be disable --now (stop + disable), not a bare stop: a stop
	// alone would let Restart=on-failure relaunch the supervisor mid-teardown.
	if len(calls) == 0 || !reflect.DeepEqual(calls[0], []string{"systemctl", "--user", "disable", "--now", "sklp-space-production.service"}) {
		t.Fatalf("first systemctl call = %v; want disable --now", calls)
	}
	// The unit file must be gone so the space does not restart at boot.
	if _, err := os.Stat(UnitPath("production")); !os.IsNotExist(err) {
		t.Fatalf("unit file still present after DisableUnit: %v", err)
	}
}

// TestEnableLinger_AlreadyOn: when loginctl reports Linger=yes, EnableLinger is
// a no-op (no enable-linger call).
func TestEnableLinger_AlreadyOn(t *testing.T) {
	t.Setenv("USER", "syl")
	restoreOut, restoreRun := runCommandOutput, runCommand
	defer func() { runCommandOutput, runCommand = restoreOut, restoreRun }()

	runCommandOutput = func(string, ...string) (string, error) { return "Linger=yes\n", nil }
	called := false
	runCommand = func(string, ...string) error { called = true; return nil }

	if err := EnableLinger(); err != nil {
		t.Fatalf("EnableLinger: %v", err)
	}
	if called {
		t.Fatal("EnableLinger called enable-linger though linger already on")
	}
}

// The hidden supervisor sits under `deploy` in sklp but at the root in sklet,
// which rejects `deploy` as an unknown verb. A unit written with the wrong argv
// looks fine on disk and crashloops at boot, so pin both spellings.
func TestRenderUnitSuperviseVerbPerBinary(t *testing.T) {
	for _, tc := range []struct{ exe, want string }{
		{"/usr/local/bin/sklp", "ExecStart=/usr/local/bin/sklp deploy __supervise --space production"},
		{"/home/u/.local/bin/sklet", "ExecStart=/home/u/.local/bin/sklet __supervise --space production"},
	} {
		unit := renderUnit("production", tc.exe, nil)
		if !strings.Contains(unit, tc.want) {
			t.Errorf("%s: unit does not contain %q\n%s", tc.exe, tc.want, unit)
		}
	}
}
