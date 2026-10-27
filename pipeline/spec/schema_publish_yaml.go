package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ScanConfigYAML is the YAML-facing shape of a `kind: publish`
// step's `scan:` field. Three forms decode into the same struct:
//
//	# absent → defaults (Enabled=true, severity=HIGH/CRITICAL,
//	#                    ignore_unfixed=true)
//	scan: false             # full opt-out (Disabled=true)
//	scan:
//	  severity: [CRITICAL]
//	  ignore_unfixed: false
//
// The pointer fields preserve the "user did not say" vs "user said X"
// distinction so the compiler can apply defaults without losing intent.
type ScanConfigYAML struct {
	// Disabled is true when the user wrote `scan: false`. Mutually
	// exclusive with the mapping form (the unmarshaller rejects both).
	Disabled bool `yaml:"-"`

	// Present is true when the user supplied any non-false scan value.
	// Lets the compiler tell `scan: { severity: [...] }` apart from an
	// absent key — both decode to defaults today but the distinction is
	// kept for future schema evolution.
	Present bool `yaml:"-"`

	// Severity is the list of Trivy severities that fail the step.
	// Empty (and not Disabled) means apply the default HIGH/CRITICAL.
	Severity []string `yaml:"severity,omitempty"`

	// IgnoreUnfixed mirrors Trivy's --ignore-unfixed. Pointer so the
	// compiler can default it to true on omission without losing an
	// explicit `false`.
	IgnoreUnfixed *bool `yaml:"ignore_unfixed,omitempty"`
}

// UnmarshalYAML accepts:
//   - a bool (only `false` is meaningful: full opt-out)
//   - a mapping with `severity` / `ignore_unfixed`
//
// Any other shape is a parse error so typos surface at load time.
func (s *ScanConfigYAML) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// Booleans are surfaced as scalars with tag !!bool.
		if node.Tag == "!!bool" || node.Value == "false" || node.Value == "true" {
			var b bool
			if err := node.Decode(&b); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			s.Present = true
			s.Disabled = !b
			return nil
		}
		return fmt.Errorf("scan: expected `false` or a mapping, got %q at line %d", node.Value, node.Line)

	case yaml.MappingNode:
		type raw ScanConfigYAML
		var r raw
		if err := node.Decode(&r); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		*s = ScanConfigYAML(r)
		s.Present = true
		s.Disabled = false
		return nil

	default:
		return fmt.Errorf("scan: expected bool or mapping, got kind %d at line %d", node.Kind, node.Line)
	}
}

// BuildSecretYAML declares one BuildKit `--secret id=...` entry for a
// `kind: publish` step. Exactly one of Env or File must be set;
// the unmarshaller enforces this so the compile path can assume the
// invariant.
type BuildSecretYAML struct {
	// ID is the BuildKit secret identifier mounted at
	// `/run/secrets/<ID>` inside the build sandbox; the Dockerfile
	// references it via `RUN --mount=type=secret,id=<ID>`.
	ID string `yaml:"id"`

	// Env names a runner-process env variable whose value docker pulls
	// at build time (`--secret id=<ID>,env=<Env>`). Mutually exclusive
	// with File.
	Env string `yaml:"env,omitempty"`

	// File is a host path docker reads at build time
	// (`--secret id=<ID>,src=<File>`). Leading `~/` is tilde-expanded
	// to `$HOME/...` at compile. Mutually exclusive with Env.
	File string `yaml:"file,omitempty"`
}

// UnmarshalYAML enforces (id required) AND (exactly one of env/file).
// Surfacing the failure at load time turns a runtime "image build
// error 30s later" into a fast compile failure with the right pointer
// at the offending entry.
func (d *BuildSecretYAML) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("secrets[]: expected a mapping at line %d", node.Line)
	}
	type raw BuildSecretYAML
	var r raw
	if err := node.Decode(&r); err != nil {
		return fmt.Errorf("secrets[]: %w", err)
	}
	if r.ID == "" {
		return fmt.Errorf("secrets[]: `id` is required (line %d)", node.Line)
	}
	hasEnv := r.Env != ""
	hasFile := r.File != ""
	if hasEnv && hasFile {
		return fmt.Errorf("secrets[%q]: set exactly one of `env` / `file`, not both", r.ID)
	}
	if !hasEnv && !hasFile {
		return fmt.Errorf("secrets[%q]: set one of `env` / `file`", r.ID)
	}
	*d = BuildSecretYAML(r)
	return nil
}

// StepSecrets is the polymorphic YAML shape for a step's `secrets:`
// field. Shell steps use the legacy `[]string` allowlist that names
// pipeline-level secrets; `kind: publish` steps use a list of
// `{ id, env|file }` mappings declaring BuildKit secrets. The two
// shapes are distinguished at parse time by the YAML node kind of the
// first entry.
type StepSecrets struct {
	// Names is the legacy allowlist form: each entry must be a name
	// declared at the pipeline level. Non-nil only when the YAML
	// supplied a list of scalars.
	Names []string `yaml:"-"`

	// Build is the BuildKit-secrets form for `kind: publish`
	// steps. Non-nil only when the YAML supplied a list of mappings.
	Build []BuildSecretYAML `yaml:"-"`
}

// UnmarshalYAML detects which of the two shapes was written. Empty
// sequences decode to an empty StepSecrets (both fields nil). A mixed
// sequence (some scalars + some mappings) is rejected so a typo cannot
// silently degrade one shape into the other.
func (s *StepSecrets) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("secrets: expected a list, got kind %d at line %d", node.Kind, node.Line)
	}
	if len(node.Content) == 0 {
		return nil
	}
	first := node.Content[0]
	switch first.Kind {
	case yaml.ScalarNode:
		var names []string
		if err := node.Decode(&names); err != nil {
			return fmt.Errorf("secrets: %w", err)
		}
		s.Names = names
		return nil
	case yaml.MappingNode:
		var entries []BuildSecretYAML
		if err := node.Decode(&entries); err != nil {
			return err
		}
		s.Build = entries
		return nil
	default:
		return fmt.Errorf("secrets: entries must be strings or mappings (line %d)", first.Line)
	}
}
