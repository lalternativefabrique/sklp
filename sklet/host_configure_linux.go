package sklet

import (
	"fmt"
	"os"
	"path/filepath"
)

// RunHostConfigure renders the agent unit from the node config and puts it
// live: the one path a node is configured through, at first boot and on every
// later change alike, so "renew the config" is a re-run rather than a hand
// edit of a unit file.
func RunHostConfigure(dryRun bool) int {
	unit, err := renderAgentUnit()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklet host configure: %v\n", err)
		return 2
	}
	if dryRun {
		fmt.Printf("# %s\n%s", AgentUnitPath(), unit)
		return 0
	}
	if err := installAgentUnit(unit); err != nil {
		fmt.Fprintf(os.Stderr, "sklet host configure: %v\n", err)
		return 1
	}
	fmt.Printf("sklet host configure: %s rendered from %s, %s restarted\n", AgentUnitPath(), NodeConfigPath(), agentUnitName)
	return 0
}

func renderAgentUnit() (string, error) {
	cfg, err := LoadNodeConfig(NodeConfigPath())
	if err != nil {
		return "", err
	}
	exe, err := agentExecutable()
	if err != nil {
		return "", err
	}
	envFile := ""
	if _, err := os.Stat(NodeEnvPath()); err == nil {
		envFile = NodeEnvPath()
	}
	return cfg.RenderAgentUnit(exe, envFile), nil
}

func installAgentUnit(unit string) error {
	path := AgentUnitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", agentUnitName},
		{"--user", "restart", agentUnitName},
	} {
		if err := run("systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %v: %w", args, err)
		}
	}
	return nil
}

// agentExecutable is what the unit starts: the launcher when it is installed,
// so the agent gets its capabilities without any on the sklet binary itself.
func agentExecutable() (string, error) {
	if home, err := os.UserHomeDir(); err == nil && LauncherReady(home) {
		return LauncherPath(home), nil
	}
	return os.Executable()
}
