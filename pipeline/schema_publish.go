package pipeline

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// BuildKitSecretEnvMissingError is returned by the compile path when a
// `kind: publish` step declares a BuildKit secret backed by an env
// var that is not set in the runner's process environment. The CLI
// compile-failure printer recognises this type and renders a
// "set <ENV> in your shell or in .env / .env.<pipeline>, then re-run"
// hint pointing at the exact variable.
type BuildKitSecretEnvMissingError struct {
	Step     string
	SecretID string
	EnvVar   string
}

func (e *BuildKitSecretEnvMissingError) Error() string {
	return fmt.Sprintf(
		"secret %q requires env var %q but it is not set in the runner's environment",
		e.SecretID, e.EnvVar,
	)
}

// BuildArgEnvMissingError is returned by the compile path when a
// `kind: publish` step's `build_args:` value references an
// env var via `${VAR}` that is not set in the layered pipeline env
// (env_files + pipeline-level env + step env + process env). Mirrors
// BuildKitSecretEnvMissingError so the CLI failure printer can render
// a precise "set <ENV> in your shell or in .env / .env.<pipeline>"
// hint pointing at the exact build_args entry.
type BuildArgEnvMissingError struct {
	Step    string
	ArgName string
	EnvVar  string
}

func (e *BuildArgEnvMissingError) Error() string {
	return fmt.Sprintf(
		"build_args[%q] references env var %q but it is not set in the runner's environment",
		e.ArgName, e.EnvVar,
	)
}

// buildArgKeyRE constrains build_args keys to the Docker ARG identifier
// shape. Anything else is rejected at compile time so a typo cannot
// produce a silently-dropped `--build-arg` (docker accepts unknown ARGs
// without a warning by default).
var buildArgKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateBuildArgKeys rejects any build_args key that does not match
// the Docker ARG identifier shape. Empty / absent map is a no-op.
func validateBuildArgKeys(stepName string, args map[string]string) error {
	for k := range args {
		if !buildArgKeyRE.MatchString(k) {
			return fmt.Errorf(
				"step %q: build_args key %q is invalid (must match [A-Za-z_][A-Za-z0-9_]*)",
				stepName, k,
			)
		}
	}
	return nil
}

// buildArgEnvRefRE matches one `${VAR}` reference (no `$VAR` shorthand
// and no default-value `${VAR:-x}` form: the schema keeps things
// boringly explicit so users never wonder which form is legal).
var buildArgEnvRefRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolateBuildArgs expands `${VAR}` references in build_args
// values against the layered env (step env > pipeline env > process
// env). Unset references fail with BuildArgEnvMissingError so the user
// catches a typo at compile time instead of `ARG NAME` resolving to an
// empty string inside the image.
//
// Returns a new map; the input is not mutated. A nil/empty input
// returns nil.
func interpolateBuildArgs(stepName string, args, pipelineEnv, stepEnv map[string]string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		var firstErr error
		expanded := buildArgEnvRefRE.ReplaceAllStringFunc(v, func(match string) string {
			name := match[2 : len(match)-1]
			if val, ok := stepEnv[name]; ok {
				return val
			}
			if val, ok := pipelineEnv[name]; ok {
				return val
			}
			if val, ok := os.LookupEnv(name); ok {
				return val
			}
			if firstErr == nil {
				firstErr = &BuildArgEnvMissingError{
					Step:    stepName,
					ArgName: k,
					EnvVar:  name,
				}
			}
			return ""
		})
		if firstErr != nil {
			return nil, firstErr
		}
		// Guard against a stray `$` that wasn't part of a recognised
		// reference but the user probably meant as one — surface it
		// instead of silently leaving the literal in the image.
		if strings.Contains(expanded, "${") {
			return nil, fmt.Errorf(
				"step %q: build_args[%q] contains an unrecognised `${...}` reference (use ${NAME} with [A-Za-z_][A-Za-z0-9_]*)",
				stepName, k,
			)
		}
		out[k] = expanded
	}
	return out, nil
}
