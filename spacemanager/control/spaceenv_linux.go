//go:build linux

package control

// spaceenv_linux.go persists the secrets a REMOTE apply resolved, so a space
// deployed over the control API comes back on its own after a restart.
//
// The local path never needed this: the operator's dotenv is named by the
// manifest's `env_files:`, the unit gets it as EnvironmentFile=, and every
// ${VAR} resolves again at boot. A remote apply has no such file — the client
// resolved the values in ITS environment and shipped them over TLS, and the
// manifest deliberately keeps only the names. Nothing on the node could resolve
// them again, so a private image failed to pull and a fresh Postgres refused to
// initialise, both until someone re-applied by hand.
//
// The values land in a 0600 file next to the manifest rather than inside it: the
// manifest is read by anything that inspects a space, while this file exists for
// exactly one reader, the supervisor's own unit.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SpaceEnvPath is the per-space environment file holding the secrets a remote
// apply resolved. Written 0600, read back by the supervisor unit.
func SpaceEnvPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "env")
}

// MergeSpaceEnv overlays vars onto the space's persisted env and writes the
// union. An apply that resolved nothing new — an image roll rebuilt from the
// on-disk manifest, whose values were expanded from this very file — must not
// drop what an earlier apply persisted: a var referenced only inside a
// composed value ("https://${HOST}") lives nowhere else on the node.
func MergeSpaceEnv(space string, vars map[string]string) error {
	existing, err := ReadSpaceEnv(space)
	if err != nil {
		return err
	}
	for k, v := range vars {
		existing[k] = v
	}
	return WriteSpaceEnv(space, existing)
}

// persistSpaceEnv writes the secrets an apply resolved.
//
// authoritative says whether vars is the COMPLETE set the space needs. A remote
// apply resolves every ${VAR} the manifest references (SpaceMeta.EnvValues), so
// its list is complete and replacing on it is what finally lets a secret
// removed from the store leave the node. An image roll rebuilds the manifest
// from disk and resolves nothing new, so its map says nothing about what the
// space needs — replacing on it would wipe every secret, which is why the union
// is still the behaviour there.
func persistSpaceEnv(space string, vars map[string]string, authoritative bool) error {
	if authoritative {
		return WriteSpaceEnv(space, vars)
	}
	return MergeSpaceEnv(space, vars)
}

// WriteSpaceEnv persists vars for space as a dotenv, 0600, atomically. An empty
// map REMOVES the file: a re-apply that no longer resolves any secret must not
// leave the previous one behind for the unit to keep sourcing.
func WriteSpaceEnv(space string, vars map[string]string) error {
	path := SpaceEnvPath(space)
	if len(vars) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove space env: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create space config dir: %w", err)
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		// systemd's EnvironmentFile takes KEY=VALUE with no quoting rules we can
		// rely on, so a value carrying a newline would inject a second variable.
		// Such a value cannot round-trip; refuse it rather than write a file that
		// silently means something else.
		if strings.ContainsAny(vars[k], "\n\r") {
			return fmt.Errorf("value of %s contains a newline and cannot be stored in an environment file", k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, vars[k])
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "env-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp space env: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp space env: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp space env: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp space env: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit space env: %w", err)
	}
	return nil
}

// RemoveSpaceEnv deletes a space's environment file at teardown, so a deleted
// space leaves no secret behind.
func RemoveSpaceEnv(space string) error {
	if err := os.Remove(SpaceEnvPath(space)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
