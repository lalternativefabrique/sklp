package pipeline

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/registry"
)

func boolPtr(b bool) *bool { return &b }

func loaderWith(hosts map[string]registry.Credential) registryLoader {
	return func() (*registry.File, error) {
		return &registry.File{Registries: hosts}, nil
	}
}

func TestPushHostsSkipsNonPushingSteps(t *testing.T) {
	p := Pipeline{Steps: []Step{
		{Name: "build", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/core:1"},
		{Name: "local", Kind: "publish", Image: "ghcr.io/x/y:1", Push: boolPtr(false)},
		{Name: "again", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/web:1"},
		{Name: "test", Run: "go test ./..."},
	}}
	got := pushHosts(p)
	want := []string{"rg.fr-par.scw.cloud"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("pushHosts = %v, want %v", got, want)
	}
}

// A bare image reference pushes to Docker Hub, so the credential must be
// looked up under docker.io rather than under an empty host that no login
// could ever name.
func TestPushHostsDefaultsBareImageToDockerHub(t *testing.T) {
	p := Pipeline{Steps: []Step{{Name: "p", Kind: "publish", Image: "org/app:1"}}}
	got := pushHosts(p)
	if len(got) != 1 || got[0] != "docker.io" {
		t.Fatalf("pushHosts = %v, want [docker.io]", got)
	}
}

func TestResolveRegistryCredentialsSucceeds(t *testing.T) {
	p := Pipeline{Steps: []Step{{Name: "p", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/core:1"}}}
	regs, err := resolveRegistryCredentials(p, loaderWith(map[string]registry.Credential{
		"rg.fr-par.scw.cloud": {Username: "nologin", Auth: registry.EncodeAuth("nologin", "pw")},
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(regs) != 1 || regs[0].Username != "nologin" || regs[0].Password != "pw" {
		t.Fatalf("resolved %+v", regs)
	}
}

// The run must fail before the first build, naming every uncovered host and
// the verb that fixes it — not after a push has already spent the build.
func TestResolveRegistryCredentialsListsEveryMissingHost(t *testing.T) {
	p := Pipeline{Steps: []Step{
		{Name: "a", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/core:1"},
		{Name: "b", Kind: "publish", Image: "ghcr.io/x/y:1"},
	}}
	_, err := resolveRegistryCredentials(p, loaderWith(nil))
	var missing *MissingCredentialError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want MissingCredentialError", err)
	}
	if len(missing.Hosts) != 2 {
		t.Fatalf("hosts = %v, want both", missing.Hosts)
	}
	msg := missing.Error()
	for _, want := range []string{
		"sklp registry login rg.fr-par.scw.cloud --provider scaleway",
		"sklp registry login ghcr.io --provider github",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// A missing credential is a compile failure: it is reported against the verb
// that fixes it, before any step runs.
func TestMissingCredentialIsACompileFailure(t *testing.T) {
	err := &MissingCredentialError{Hosts: []string{"ghcr.io"}}
	if !isCompileFailure(err) {
		t.Error("MissingCredentialError should be a compile failure")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Error("MissingCredentialError should wrap ErrInvalidConfig")
	}
}

func TestResolveRegistryCredentialsNoPushNeedsNothing(t *testing.T) {
	p := Pipeline{Steps: []Step{{Name: "t", Run: "go test ./..."}}}
	regs, err := resolveRegistryCredentials(p, loaderWith(nil))
	if err != nil {
		t.Fatalf("a pipeline that pushes nothing needs no credential: %v", err)
	}
	if len(regs) != 0 {
		t.Fatalf("regs = %+v, want none", regs)
	}
}

// Every run hands its credential to the push path: nothing is skipped because
// something already mentions the host, so a revoked secret fails against the
// registry's own answer instead of being silently preferred.
func TestPerformRegistryLoginAlwaysLogsInOncePerHost(t *testing.T) {
	var calls []string
	login := func(host, username, password string) error {
		calls = append(calls, host+"/"+username+"/"+password)
		return nil
	}
	r := &resolvedRegistry{Host: "ghcr.io", Username: "oauth2", Password: "tok"}
	once := newLoginOnce()
	var log bytes.Buffer
	for i := 0; i < 3; i++ {
		if err := performRegistryLogin(r, once, login, &log); err != nil {
			t.Fatalf("login: %v", err)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("logins = %v, want exactly one", calls)
	}
	if calls[0] != "ghcr.io/oauth2/tok" {
		t.Errorf("login got %q", calls[0])
	}
	if strings.Contains(log.String(), "tok") {
		t.Errorf("the password leaked into the log: %s", log.String())
	}
}

func TestImageHost(t *testing.T) {
	cases := map[string]string{
		"rg.fr-par.scw.cloud/acme/core:1": "rg.fr-par.scw.cloud",
		"ghcr.io/org/app:2":               "ghcr.io",
		"localhost:5000/app:3":            "localhost:5000",
		"org/app:1":                       "",
		"app":                             "",
	}
	for in, want := range cases {
		if got := imageHost(in); got != want {
			t.Errorf("imageHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func ciPipeline() Pipeline {
	return Pipeline{
		Registries: map[string]RegistryCredential{
			"rg.fr-par.scw.cloud": {Username: "nologin", Password: "${SCW_SECRET_KEY}"},
		},
		Steps: []Step{{Name: "p", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/core:1"}},
	}
}

func envWith(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestCIRegistryResolvesFromBlockAndEnv(t *testing.T) {
	regs, err := resolveCIRegistryCredentials(ciPipeline(), envWith(map[string]string{"SCW_SECRET_KEY": "pw"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(regs) != 1 || regs[0].Username != "nologin" || regs[0].Password != "pw" {
		t.Fatalf("resolved %+v", regs)
	}
}

// A CI VM holds no login: an unset or empty variable fails the run, it never
// falls back to a file an operator may have left there.
func TestCIRegistryEmptyVarIsAnError(t *testing.T) {
	for name, env := range map[string]map[string]string{"unset": {}, "empty": {"SCW_SECRET_KEY": ""}} {
		_, err := resolveCIRegistryCredentials(ciPipeline(), envWith(env))
		var ciErr *CIRegistryError
		if !errors.As(err, &ciErr) {
			t.Fatalf("%s: err = %v, want CIRegistryError", name, err)
		}
		if ciErr.EmptyVar["rg.fr-par.scw.cloud"] != "SCW_SECRET_KEY" {
			t.Fatalf("%s: %+v", name, ciErr)
		}
		if !strings.Contains(err.Error(), "SCW_SECRET_KEY") || !isCompileFailure(err) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCIRegistryUndeclaredHostIsAnError(t *testing.T) {
	p := ciPipeline()
	p.Steps = append(p.Steps, Step{Name: "g", Kind: "publish", Image: "ghcr.io/x/y:1"})
	_, err := resolveCIRegistryCredentials(p, envWith(map[string]string{"SCW_SECRET_KEY": "pw"}))
	var ciErr *CIRegistryError
	if !errors.As(err, &ciErr) || len(ciErr.Undeclared) != 1 || ciErr.Undeclared[0] != "ghcr.io" {
		t.Fatalf("err = %v", err)
	}
}

// Outside CI the block is ignored: a dev pushes with their own login even when
// the variable happens to be set in their shell.
func TestPushCredentialsDevIgnoresBlock(t *testing.T) {
	regs, err := resolvePushCredentials(ciPipeline(), false, loaderWith(map[string]registry.Credential{
		"rg.fr-par.scw.cloud": {Username: "nologin", Auth: registry.EncodeAuth("nologin", "mine")},
	}), envWith(map[string]string{"SCW_SECRET_KEY": "shared"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if regs[0].Password != "mine" {
		t.Fatalf("password = %q, want the login file's", regs[0].Password)
	}
	_, err = resolvePushCredentials(ciPipeline(), false, loaderWith(nil), envWith(map[string]string{"SCW_SECRET_KEY": "shared"}))
	var missing *MissingCredentialError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want MissingCredentialError", err)
	}
}

// In CI the login file is never read, even when it covers the host.
func TestPushCredentialsCIIgnoresLoginFile(t *testing.T) {
	p := Pipeline{Steps: []Step{{Name: "p", Kind: "publish", Image: "rg.fr-par.scw.cloud/acme/core:1"}}}
	_, err := resolvePushCredentials(p, true, loaderWith(map[string]registry.Credential{
		"rg.fr-par.scw.cloud": {Username: "nologin", Auth: registry.EncodeAuth("nologin", "leftover")},
	}), envWith(nil))
	var ciErr *CIRegistryError
	if !errors.As(err, &ciErr) {
		t.Fatalf("err = %v, want CIRegistryError", err)
	}
}

// On a dev machine the registry password variable is not required: the push
// authenticates from the login file, so a pipeline that declares the variable
// under `secrets:` still runs without it.
func TestRegistryPasswordVarIsOptionalOutsideCI(t *testing.T) {
	t.Setenv("SKLP_TEST_REG_PW", "")
	out, err := resolveSecrets([]string{"SKLP_TEST_REG_PW"}, t.TempDir(), registryPasswordVars(Pipeline{
		Registries: map[string]RegistryCredential{"h": {Username: "u", Password: "${SKLP_TEST_REG_PW}"}},
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, present := out["SKLP_TEST_REG_PW"]; present {
		t.Fatalf("an absent optional secret must not be exported: %v", out)
	}
	if _, err := resolveSecrets([]string{"SKLP_TEST_REG_PW"}, t.TempDir(), nil); err == nil {
		t.Fatal("without the optional set the secret is still required")
	}
}
