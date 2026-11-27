// Package grants records which deployed services the node operator lets drive
// this node's control API from inside their container: a dashboard that runs
// on the node it manages. A grant is written only on the node (`sklet console
// allow`), never by a manifest, and holds the space's owner at the time it was
// given, so a space recreated under the same name by someone else inherits
// nothing.
package grants

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

type Scope string

const (
	Read    Scope = "read"
	Operate Scope = "operate"
	Admin   Scope = "admin"
)

// MountPoint is where a granted service finds the socket, as SocketName.
const (
	MountPoint = "/run/sklp"
	SocketName = "control.sock"
)

type Grant struct {
	Space     string    `json:"space"`
	Service   string    `json:"service"`
	Owner     string    `json:"owner"`
	Scope     Scope     `json:"scope"`
	GrantedAt time.Time `json:"granted_at"`
}

func (g Grant) Key() string { return g.Space + "/" + g.Service }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func ValidName(name string) bool { return nameRE.MatchString(name) }

func ParseScope(s string) (Scope, error) {
	switch Scope(s) {
	case Read, Operate, Admin:
		return Scope(s), nil
	}
	return "", fmt.Errorf("scope %q: want read, operate or admin", s)
}

func configRoot() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config")
	}
	return filepath.Join(home, ".config")
}

func Path() string { return filepath.Join(configRoot(), "sklp", "control-grants.json") }

// Dir is the host directory bind-mounted at MountPoint in the granted
// service: the directory and not the socket file, so a socket the agent
// recreates on restart shows up in a container that is already running.
func Dir(space, service string) string {
	return filepath.Join(configRoot(), "sklp", "grants", space+"."+service)
}

func Load() ([]Grant, error) {
	data, err := os.ReadFile(Path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Grant
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	valid := out[:0]
	for _, g := range out {
		if ValidName(g.Space) && ValidName(g.Service) {
			if _, err := ParseScope(string(g.Scope)); err == nil {
				valid = append(valid, g)
			}
		}
	}
	return valid, nil
}

func Find(space, service string) (Grant, bool) {
	all, err := Load()
	if err != nil {
		return Grant{}, false
	}
	for _, g := range all {
		if g.Space == space && g.Service == service {
			return g, true
		}
	}
	return Grant{}, false
}

func Put(g Grant) error {
	if !ValidName(g.Space) || !ValidName(g.Service) {
		return fmt.Errorf("grant %s: invalid space or service name", g.Key())
	}
	if _, err := ParseScope(string(g.Scope)); err != nil {
		return err
	}
	if g.Owner == "" {
		return fmt.Errorf("grant %s: the space records no owner", g.Key())
	}
	all, err := Load()
	if err != nil {
		return err
	}
	kept := []Grant{g}
	for _, o := range all {
		if o.Key() != g.Key() {
			kept = append(kept, o)
		}
	}
	return save(kept)
}

func Remove(space, service string) (bool, error) {
	all, err := Load()
	if err != nil {
		return false, err
	}
	kept := all[:0]
	found := false
	for _, g := range all {
		if g.Space == space && g.Service == service {
			found = true
			continue
		}
		kept = append(kept, g)
	}
	if !found {
		return false, nil
	}
	return true, save(kept)
}

func save(all []Grant) error {
	sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		return err
	}
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}
