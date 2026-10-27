package pipeline

import "github.com/lalternativefabrique/sklp/pipeline/spec"

// The pipeline contract lives in spec so that consumers which only read a
// pipeline (auto-selection, schema generation, core's webhook routing) do
// not link the execution engine — and with it the embedded OCI runtime.
// These aliases keep the engine's own API unchanged.
type (
	Pipeline     = spec.Pipeline
	RequireBlock = spec.RequireBlock
	Step         = spec.Step
	Service      = spec.Service
	Ready        = spec.Ready
	CacheEntry   = spec.CacheEntry

	ExtendsList        = spec.ExtendsList
	ScopeRef           = spec.ScopeRef
	TagBlock           = spec.TagBlock
	ScanConfigYAML     = spec.ScanConfigYAML
	BuildSecretYAML    = spec.BuildSecretYAML
	StepSecrets        = spec.StepSecrets
	RegistryCredential = spec.RegistryCredential
)

var ErrTemplateNotRunnable = spec.ErrTemplateNotRunnable

func LoadPipeline(path string) (Pipeline, error) { return spec.LoadPipeline(path) }

func ScopeMatches(stepScopes []string, scopeDefs map[string][]string, changed []string) (bool, error) {
	return spec.ScopeMatches(stepScopes, scopeDefs, changed)
}
func Builtin(name string) ([]byte, bool) { return spec.Builtin(name) }
