package devstack

import (
	"sort"
	"strings"
	"testing"
)

// TestComposeEnv_NoInterpolation pins that `sklp dev`'s composeEnv does no
// per-service host-port templating: any `${...}` reference reaches the child
// verbatim (host exposure is the hostname mux now). Siblings are addressed by
// space DNS name (e.g. `core:4101`) written directly in the config.
func TestComposeEnv_NoInterpolation(t *testing.T) {
	pipelineEnv := map[string]string{"FROM_PIPE": "p"}
	svcEnv := map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db",
		"PG_PORT":      "${SOME_PORT_REF}",
		"UNTOUCHED":    "no refs here",
	}
	m := envSliceToMap(composeEnv(nil, pipelineEnv, svcEnv))
	if m["DATABASE_URL"] != "postgres://u:p@localhost:5432/db" {
		t.Errorf("DATABASE_URL = %q", m["DATABASE_URL"])
	}
	if m["PG_PORT"] != "${SOME_PORT_REF}" {
		t.Errorf("PG_PORT must stay verbatim (no templating), got %q", m["PG_PORT"])
	}
	if m["UNTOUCHED"] != "no refs here" {
		t.Errorf("UNTOUCHED = %q", m["UNTOUCHED"])
	}
}

// TestComposeEnv_NonSKLPRefsLeftAlone makes sure the narrow scope of
// the interpolation pass doesn't silently expand arbitrary `${VAR}`
// references (which would surprise users and conflict with shell
// expansion inside `run:` commands).
func TestComposeEnv_NonSKLPRefsLeftAlone(t *testing.T) {
	pipelineEnv := map[string]string{"FOO": "bar"}
	svcEnv := map[string]string{"X": "${FOO}-suffix"}
	got := composeEnv(nil, pipelineEnv, svcEnv)
	m := envSliceToMap(got)
	if m["X"] != "${FOO}-suffix" {
		t.Errorf("X expanded unexpectedly: %q", m["X"])
	}
}

// TestComposeContainerEnv_OmitsHostEnv is the regression guard for the runc
// PATH leak: the container env must come ONLY from env files / pipeline /
// service, never from os.Environ(). A host-only variable (and a host PATH)
// must NOT appear, while the declared sources do — verbatim (no templating).
func TestComposeContainerEnv_OmitsHostEnv(t *testing.T) {
	t.Setenv("SKLP_HOST_ONLY_PROBE", "leaked")
	t.Setenv("PATH", "/home/u/.cargo/bin:/usr/bin")

	envFiles := map[string]string{"FROM_FILE": "f"}
	pipelineEnv := map[string]string{"FROM_PIPE": "p"}
	svcEnv := map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/db",
		"FROM_SVC":     "s",
	}
	m := envSliceToMap(composeContainerEnv(envFiles, pipelineEnv, svcEnv))

	if _, ok := m["SKLP_HOST_ONLY_PROBE"]; ok {
		t.Error("host-only env var leaked into container env")
	}
	if _, ok := m["PATH"]; ok {
		t.Errorf("host PATH leaked into container env: %q", m["PATH"])
	}
	if m["FROM_FILE"] != "f" || m["FROM_PIPE"] != "p" || m["FROM_SVC"] != "s" {
		t.Errorf("declared sources missing: %v", m)
	}
	if m["DATABASE_URL"] != "postgres://u:p@localhost:5432/db" {
		t.Errorf("DATABASE_URL must reach the container verbatim, got %q", m["DATABASE_URL"])
	}
}

// TestComposeEnv_StillIncludesHostEnv pins the legacy host-process path: it
// must keep inheriting os.Environ() (PATH and all), unchanged by the refactor.
func TestComposeEnv_StillIncludesHostEnv(t *testing.T) {
	t.Setenv("SKLP_HOST_ONLY_PROBE", "present")
	m := envSliceToMap(composeEnv(nil, nil, nil))
	if m["SKLP_HOST_ONLY_PROBE"] != "present" {
		t.Error("legacy composeEnv no longer inherits host env")
	}
}

func envSliceToMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		m[kv[:eq]] = kv[eq+1:]
	}
	// keys are unique by construction; sort here for any external diffs
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return m
}
