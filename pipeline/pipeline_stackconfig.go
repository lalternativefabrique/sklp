package pipeline

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// pipeline_stackconfig.go loads the repo's Pulumi-style config into the run's
// environment, the same source `sklp deploy` already reads.
//
// A dotenv holds its secrets in cleartext, so it can only ever be gitignored —
// which means every machine and every runner needs its own out-of-band copy.
// stackconfig keeps plain values readable and secrets sealed under
// SKLP_SECRETS_PASSPHRASE in one committable file, so CI needs the passphrase
// alone rather than the whole set — and only for the secrets the running
// pipeline declares.
//
// Precedence matches env_files exactly: host env wins, so an explicitly
// exported value is never overridden by a checked-in file.

// StackConfigFile is the repo-level config, shared by every pipeline: one ci-*
// pipeline routinely builds several stacks, so a repo-wide file is the layer
// that matches a pipeline's scope.
const StackConfigFile = ".sklp/config.yaml"

// StackConfigDirGlob matches the per-stack files `sklp config set --stack <s>`
// writes (.sklp/<stack>/config.yaml). CI reads them too: without this, a value
// stored through the CLI would be invisible to the runs meant to consume it.
const StackConfigDirGlob = ".sklp/*/config.yaml"

// stackConfigPaths lists the config files to load, repo-wide first then each
// per-stack file in sorted order. Later files win among themselves (host env
// still outranks all of them), so a stack can specialise a repo-wide value.
func stackConfigPaths(repoRoot string) ([]string, error) {
	var paths []string
	root := filepath.Join(repoRoot, StackConfigFile)
	if _, err := os.Stat(root); err == nil {
		paths = append(paths, root)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stack config %s: %w", StackConfigFile, err)
	}

	matches, err := filepath.Glob(filepath.Join(repoRoot, StackConfigDirGlob))
	if err != nil {
		return nil, fmt.Errorf("stack config %s: %w", StackConfigDirGlob, err)
	}
	sort.Strings(matches)
	return append(paths, matches...), nil
}

// LoadPipelineStackConfig resolves the repo's stack config files and injects
// their values into the process env, leaving already-set variables untouched. No
// config file at all is a no-op: the mechanism is opt-in, and a repo may still
// be on env_files only.
//
// declared is the pipeline's `secrets:` list, and it decides which encrypted
// entries are decrypted at all. A secret the pipeline never declares is left
// sealed, so a run that reads none of them needs no passphrase — a lint/test
// pipeline is not asked for the key to the deploy vault. A DECLARED secret
// with no usable passphrase stays a hard error rather than a silent skip:
// dropping it here would surface much later as an unexplained empty
// credential inside a step.
func LoadPipelineStackConfig(repoRoot string, declared []string, stderr io.Writer) (map[string]string, error) {
	paths, err := stackConfigPaths(repoRoot)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]bool, len(declared))
	for _, name := range declared {
		wanted[name] = true
	}
	// Snapshot which names the host already exports, BEFORE any injection. Once
	// a file has os.Setenv'd a key, os.LookupEnv can no longer tell an operator
	// export from this loader's own write.
	fromHost := map[string]bool{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			fromHost[kv[:i]] = true
		}
	}

	applied := map[string]string{}
	for _, path := range paths {
		rel, rerr := filepath.Rel(repoRoot, path)
		if rerr != nil {
			rel = path
		}
		if err := loadOneStackConfig(path, rel, repoRoot, wanted, applied, fromHost, stderr); err != nil {
			return nil, err
		}
	}
	if len(applied) == 0 {
		return nil, nil
	}
	return applied, nil
}

// loadOneStackConfig injects one file's values, recording them in applied.
//
// fromHost holds the names the operator exported before any injection. A name
// in it is never overwritten. A name absent from it may be replaced by a later
// file, which is what lets a per-stack file specialise a repo-wide value —
// os.LookupEnv alone cannot express that distinction, since by then this loader
// has itself put the earlier value in the environment.
func loadOneStackConfig(path, rel, repoRoot string, wanted map[string]bool, applied map[string]string, fromHost map[string]bool, stderr io.Writer) error {
	cfg, err := stackconfig.Load(path)
	if err != nil {
		return fmt.Errorf("stack config %s: %w", rel, err)
	}
	if len(cfg.Keys()) == 0 {
		return nil
	}
	// A name the host already exports — a project secret the CI node injected,
	// or an operator's own export — is never decrypted from this file: it is
	// already a real value, and asking for the passphrase to unseal a name
	// nobody will read from here would fail every remote run that supplies
	// its secrets through core instead of this file.
	stillWanted := make(map[string]bool, len(wanted))
	for name, want := range wanted {
		if want && !fromHost[name] {
			stillWanted[name] = true
		}
	}
	pp, _, perr := stackconfig.ResolvePassphrase(repoRoot)
	if perr != nil {
		return fmt.Errorf("stack config %s: %w", rel, perr)
	}
	resolved, err := cfg.ResolveWanted(pp, stillWanted)
	if err != nil {
		return fmt.Errorf("stack config %s: %w (set %s)", rel, err, stackconfig.PassphraseEnv)
	}
	for k, v := range resolved {
		if fromHost[k] {
			applied[k] = os.Getenv(k)
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("stack config: setenv %s: %w", k, err)
		}
		applied[k] = v
	}
	// A wanted secret skipped above for being fromHost never entered resolved
	// (ResolveWanted only decrypts stillWanted), so it must still be recorded
	// here — the caller's applied map is otherwise missing it entirely.
	for name := range wanted {
		if fromHost[name] && cfg.IsSecret(name) {
			applied[name] = os.Getenv(name)
		}
	}
	if stderr != nil {
		fmt.Fprintf(stderr, "[sklp] loaded config from %s (%d variables, %d of %d secrets decrypted)\n",
			rel, len(resolved), decryptedCount(cfg, wanted), secretCount(cfg))
	}
	return nil
}

// decryptedCount reports how many of the file's secrets this run actually
// unsealed, so the log line distinguishes "no secrets here" from "secrets
// left sealed because this pipeline declares none of them".
func decryptedCount(cfg *stackconfig.Config, wanted map[string]bool) int {
	n := 0
	for _, k := range cfg.Keys() {
		if cfg.IsSecret(k) && wanted[k] {
			n++
		}
	}
	return n
}

func secretCount(cfg *stackconfig.Config) int {
	n := 0
	for _, k := range cfg.Keys() {
		if cfg.IsSecret(k) {
			n++
		}
	}
	return n
}
