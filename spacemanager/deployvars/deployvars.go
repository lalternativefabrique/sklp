// Package deployvars gathers the ${VAR} values a deploy.yaml needs on the
// operator's machine, without parsing the manifest itself: the node does that.
// It builds on every OS, so a remote deploy needs nothing Linux-only.
package deployvars

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/registry"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
	"gopkg.in/yaml.v3"
)

// SpacePlaceholder is substituted by the node per space, never by the sender.
const SpacePlaceholder = "SPACE"

var BracedVar = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// RepoRoot is the parent of the nearest .sklp segment above the deploy file,
// or the file's own dir when it is not under a .sklp tree.
func RepoRoot(path string) string {
	dir := filepath.Dir(path)
	for d := dir; ; {
		if filepath.Base(d) == ".sklp" {
			return filepath.Dir(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return dir
}

// EnvFiles reads env_files from the un-expanded YAML: their paths must not
// depend on the env they load.
func EnvFiles(raw []byte) ([]string, error) {
	var probe struct {
		EnvFiles []string `yaml:"env_files"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse deploy file (env_files): %w", err)
	}
	return probe.EnvFiles, nil
}

// LoadEnvFiles sets every key of the dotenv files that the process env does
// not already hold. A missing file is skipped.
func LoadEnvFiles(paths []string, baseDir string) error {
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(baseDir, p)
		}
		f, err := os.Open(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("env_files: open %s: %w", p, err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if k == "" {
				continue
			}
			if _, exists := os.LookupEnv(k); exists {
				continue
			}
			if err := os.Setenv(k, v); err != nil {
				f.Close()
				return fmt.Errorf("env_files: setenv %s: %w", k, err)
			}
		}
		cerr := sc.Err()
		f.Close()
		if cerr != nil {
			return fmt.Errorf("env_files: scan %s: %w", p, cerr)
		}
	}
	return nil
}

// LoadStackConfigs loads .sklp/config.yaml then the stack's own config.yaml
// into the process env, the stack file overriding the shared one. A name the
// host env held before the first load wins over both, so the snapshot is
// taken before any write.
func LoadStackConfigs(repoRoot, stackDir string) error {
	fromHost := make(map[string]bool, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			fromHost[kv[:i]] = true
		}
	}
	paths := []string{filepath.Join(repoRoot, ".sklp", "config.yaml")}
	if stackConfig := filepath.Join(stackDir, "config.yaml"); stackConfig != paths[0] {
		paths = append(paths, stackConfig)
	}
	for _, path := range paths {
		if err := loadStackConfig(path, repoRoot, fromHost); err != nil {
			return err
		}
	}
	return nil
}

func loadStackConfig(path, repoRoot string, fromHost map[string]bool) error {
	c, err := stackconfig.Load(path)
	if err != nil {
		return fmt.Errorf("stack config: %w", err)
	}
	if len(c.Keys()) == 0 {
		return nil
	}
	pp, _, perr := stackconfig.ResolvePassphrase(repoRoot)
	if perr != nil {
		return fmt.Errorf("stack config %s: %w", path, perr)
	}
	resolved, err := c.Resolve(pp)
	if err != nil {
		return fmt.Errorf("stack config %s: %w", path, err)
	}
	for k, v := range resolved {
		if fromHost[k] {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("stack config: setenv %s: %w", k, err)
		}
	}
	return nil
}

// LoadSources puts the env_files and the stack configs of the deploy file at
// path into the process env, in the order a local deploy has always used.
func LoadSources(path string, raw []byte) error {
	files, err := EnvFiles(raw)
	if err != nil {
		return err
	}
	base := RepoRoot(path)
	if err := LoadEnvFiles(files, base); err != nil {
		return err
	}
	return LoadStackConfigs(base, filepath.Dir(path))
}

// RegistryPasswordVars maps each registry host to the variable its password
// names, for passwords written as a single ${VAR}.
func RegistryPasswordVars(raw []byte) (map[string]string, error) {
	var probe struct {
		Registries map[string]struct {
			Password string `yaml:"password"`
		} `yaml:"registries"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse deploy file (registries): %w", err)
	}
	out := map[string]string{}
	for host, r := range probe.Registries {
		if m := BracedVar.FindStringSubmatch(r.Password); m != nil && r.Password == m[0] {
			out[host] = r.Password[2 : len(r.Password)-1]
		}
	}
	return out, nil
}

// HostByPasswordVar inverts RegistryPasswordVars.
func HostByPasswordVar(varByHost map[string]string) map[string]string {
	out := make(map[string]string, len(varByHost))
	for host, v := range varByHost {
		out[v] = host
	}
	return out
}

// LoginFileLookup wraps an env lookup so a registry password variable the env
// does not define is taken from the `sklp registry login` file for its host.
// A node or a CI run has no login file, so there the base lookup is all there is.
func LoginFileLookup(hostByVar map[string]string, base func(string) (string, bool)) func(string) (string, bool) {
	if len(hostByVar) == 0 {
		return base
	}
	var file *registry.File
	return func(name string) (string, bool) {
		if v, ok := base(name); ok {
			return v, true
		}
		host, isRegistryVar := hostByVar[name]
		if !isRegistryVar {
			return "", false
		}
		if file == nil {
			f, _, err := registry.Load()
			if err != nil {
				return "", false
			}
			file = f
		}
		r, err := file.Resolve(host)
		if err != nil || r.Password == "" {
			return "", false
		}
		return r.Password, true
	}
}

// Referenced lists the variables the raw YAML names, ${SPACE} aside. Like the
// expansion it feeds, it scans the whole text, comments included.
func Referenced(raw []byte) []string {
	seen := map[string]bool{}
	var names []string
	for _, ref := range BracedVar.FindAllString(string(raw), -1) {
		name := ref[2 : len(ref)-1]
		if name == SpacePlaceholder || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Collect resolves every variable the raw YAML references through lookup,
// registry passwords falling back to the login file, and fails naming every
// variable left unset.
func Collect(raw []byte, lookup func(string) (string, bool)) (map[string]string, error) {
	pwVars, err := RegistryPasswordVars(raw)
	if err != nil {
		return nil, err
	}
	lookup = LoginFileLookup(HostByPasswordVar(pwVars), lookup)
	names := Referenced(raw)
	vars := make(map[string]string, len(names))
	var missing []string
	for _, name := range names {
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			continue
		}
		vars[name] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("deploy file references unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return vars, nil
}
