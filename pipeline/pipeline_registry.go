// Package pipeline — registry credentials for a run's pushes.
//
// On a dev machine they come from the file `sklp registry login` writes
// ($XDG_CONFIG_HOME/sklp/registries.yaml); the pipeline block is ignored, so a
// secret sitting in a dev's .env never pushes in their place. On a CI node
// (SKLP_CI / CI=true) they come from the pipeline `registries:` block alone,
// its ${VAR} resolved from the run's environment; the login file is never
// read there. ~/.docker/config.json is read nowhere.
//
// Resolution happens before the first step compiles, so a missing credential
// is reported against the verb that fixes it rather than against a push that
// already spent the build.
package pipeline

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/internal/registry"
)

// resolvedRegistry is one host's login, materialised from the registries file.
type resolvedRegistry struct {
	Host     string
	Username string
	Password string
}

// MissingCredentialError names the hosts a run must push to that the
// registries file does not cover, and the verb that fixes each one. Reported
// as a single error so a pipeline pushing to several registries is fixed in
// one pass.
type MissingCredentialError struct {
	Hosts []string
}

func (e *MissingCredentialError) Error() string {
	var b strings.Builder
	b.WriteString("registry: no credential for ")
	b.WriteString(strings.Join(quoteAll(e.Hosts), ", "))
	b.WriteString("\n")
	for _, h := range e.Hosts {
		fmt.Fprintf(&b, "  run: sklp registry login %s%s\n", h, providerHintFor(h))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *MissingCredentialError) Unwrap() error { return ErrInvalidConfig }

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

// providerHintFor appends the --provider shortcut for a host the login verb
// knows, so the suggested command is complete rather than a template.
func providerHintFor(host string) string {
	switch {
	case strings.HasPrefix(host, "rg.") && strings.HasSuffix(host, ".scw.cloud"):
		return " --provider scaleway"
	case host == "ghcr.io":
		return " --provider github"
	case host == "docker.io":
		return " --provider docker-hub"
	default:
		return " --username <user>"
	}
}

// imageHost extracts the registry host from a docker image reference.
// An empty return value means the image targets the default Docker
// Hub (a bare `repo` or `org/repo` with no explicit host).
//
// Docker's rule: the first `/`-separated segment is a host iff it
// contains a `.`, a `:` (port), or equals `localhost`. Otherwise the
// segment is the first path component of an implicit Docker Hub
// reference.
func imageHost(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	slash := strings.Index(image, "/")
	if slash < 0 {
		return ""
	}
	first := image[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return ""
}

// pushHosts lists the registry hosts a run will push to, in the order the
// steps declare them and without duplicates. A step with `push: false` is
// skipped: it needs no credential.
func pushHosts(p Pipeline) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, s := range p.Steps {
		if s.Kind != "publish" {
			continue
		}
		if s.Push != nil && !*s.Push {
			continue
		}
		host := imageHost(s.Image)
		if host == "" {
			host = "docker.io"
		}
		if _, dup := seen[host]; dup {
			continue
		}
		seen[host] = struct{}{}
		out = append(out, host)
	}
	return out
}

// resolveRegistryCredentials materialises a login for every host the run
// pushes to. Returns MissingCredentialError listing every uncovered host, so
// the run fails before the first build rather than after it.
func resolveRegistryCredentials(p Pipeline, load registryLoader) ([]*resolvedRegistry, error) {
	hosts := pushHosts(p)
	if len(hosts) == 0 {
		return nil, nil
	}
	if load == nil {
		load = defaultRegistryLoader
	}
	f, err := load()
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}

	var missing []string
	out := make([]*resolvedRegistry, 0, len(hosts))
	for _, host := range hosts {
		r, err := f.Resolve(host)
		if err != nil {
			missing = append(missing, host)
			continue
		}
		out = append(out, &resolvedRegistry{Host: r.Host, Username: r.Username, Password: r.Password})
	}
	if len(missing) > 0 {
		return nil, &MissingCredentialError{Hosts: missing}
	}
	return out, nil
}

// registryLoader reads the stored credentials. Stubbed in tests; production
// reads the file `sklp registry login` writes.
type registryLoader func() (*registry.File, error)

func defaultRegistryLoader() (*registry.File, error) {
	f, _, err := registry.Load()
	return f, err
}

// registryLogin materialises one credential where the push path reads it.
type registryLogin func(host, username, password string) error

// loginOnce serialises credential writes per host within a single run so
// concurrent publish steps don't race the same auth-file write.
type loginOnce struct {
	mu   sync.Mutex
	done map[string]struct{}
}

func newLoginOnce() *loginOnce { return &loginOnce{done: map[string]struct{}{}} }

func (l *loginOnce) doneFor(host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.done[host]
	return ok
}

func (l *loginOnce) mark(host string) {
	l.mu.Lock()
	l.done[host] = struct{}{}
	l.mu.Unlock()
}

// performRegistryLogin hands one resolved credential to the push path. Every
// run writes the credential it resolved: nothing is skipped because a file
// already mentions the host, so a revoked secret fails against the registry's
// own answer instead of being silently preferred. Credentials are NEVER logged.
func performRegistryLogin(r *resolvedRegistry, once *loginOnce, login registryLogin, logW io.Writer) error {
	if r == nil {
		return nil
	}
	if once != nil && once.doneFor(r.Host) {
		return nil
	}
	if login == nil {
		login = defaultRegistryLogin
	}
	if err := login(r.Host, r.Username, r.Password); err != nil {
		return fmt.Errorf("registry: auth %s failed: %w", r.Host, err)
	}
	plan.MarkRegistryLoginWritten()
	if once != nil {
		once.mark(r.Host)
	}
	fmt.Fprintf(logW, "[sklp] registry: authenticated against %s as %s\n", r.Host, r.Username)
	return nil
}

// defaultRegistryLogin publishes one credential to the in-process store the
// push path reads. It writes no file: the secret lives only in this run's
// memory, so nothing is left on disk for a later process to pick up.
func defaultRegistryLogin(host, username, password string) error {
	registry.Remember(host, username, password)
	return nil
}

// CIRegistryError names the push hosts a CI run cannot authenticate to. On a
// CI node there is no login to fall back on, so every host must be covered by
// the pipeline `registries:` block and its ${VAR} must resolve non-empty.
type CIRegistryError struct {
	Undeclared []string
	EmptyVar   map[string]string
}

func (e *CIRegistryError) Error() string {
	var b strings.Builder
	b.WriteString("registry: CI run cannot authenticate its pushes\n")
	for _, h := range e.Undeclared {
		fmt.Fprintf(&b, "  %q: not in the pipeline `registries:` block\n", h)
	}
	hosts := make([]string, 0, len(e.EmptyVar))
	for h := range e.EmptyVar {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		fmt.Fprintf(&b, "  %q: %s is unset or empty in the run's environment (core fills it from the stack's Registry tab)\n", h, e.EmptyVar[h])
	}
	return strings.TrimRight(b.String(), "\n")
}

func (e *CIRegistryError) Unwrap() error { return ErrInvalidConfig }

var registryPasswordRefRE = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// resolveCIRegistryCredentials is the CI-node counterpart of
// resolveRegistryCredentials: the login file is never consulted, so a leftover
// operator login on the VM can never push a CI build.
func resolveCIRegistryCredentials(p Pipeline, lookup func(string) (string, bool)) ([]*resolvedRegistry, error) {
	hosts := pushHosts(p)
	if len(hosts) == 0 {
		return nil, nil
	}
	failure := &CIRegistryError{EmptyVar: map[string]string{}}
	out := make([]*resolvedRegistry, 0, len(hosts))
	for _, host := range hosts {
		spec, ok := p.Registries[host]
		if !ok || spec.Username == "" {
			failure.Undeclared = append(failure.Undeclared, host)
			continue
		}
		password := spec.Password
		if m := registryPasswordRefRE.FindStringSubmatch(spec.Password); m != nil {
			password, _ = lookup(m[1])
			if password == "" {
				failure.EmptyVar[host] = m[1]
				continue
			}
		}
		if password == "" {
			failure.EmptyVar[host] = "password"
			continue
		}
		out = append(out, &resolvedRegistry{Host: host, Username: spec.Username, Password: password})
	}
	if len(failure.Undeclared) > 0 || len(failure.EmptyVar) > 0 {
		return nil, failure
	}
	return out, nil
}

// resolvePushCredentials picks the source by where the run executes: the
// pipeline block on a CI node, the login file anywhere else.
func resolvePushCredentials(p Pipeline, ci bool, load registryLoader, lookup func(string) (string, bool)) ([]*resolvedRegistry, error) {
	if ci {
		return resolveCIRegistryCredentials(p, lookup)
	}
	return resolveRegistryCredentials(p, load)
}

// registryPasswordVars lists the ${VAR} names the pipeline `registries:` block
// references as passwords.
func registryPasswordVars(p Pipeline) map[string]bool {
	out := map[string]bool{}
	for _, spec := range p.Registries {
		if m := registryPasswordRefRE.FindStringSubmatch(spec.Password); m != nil {
			out[m[1]] = true
		}
	}
	return out
}
