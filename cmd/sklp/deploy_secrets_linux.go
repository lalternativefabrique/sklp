package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// loadProjectSecretsIntoEnv reads deploy.yaml's project_id: (if any), fetches
// every deploy-readable secret for that project from core, and sets each one
// in the process env — the same channel env_files and stack config already
// use, so ${VAR} in the deploy file resolves without the operator re-entering
// a value already saisi in the web UI. Host env still wins: an already-set
// var is left untouched, matching env_files/stack-config precedence.
//
// Best-effort by design: a deploy must not hard-fail just because the
// operator isn't logged in or core is unreachable — a secret available
// another way (host env, env_files, stack config) still resolves, and one
// that ISN'T fails later at ${VAR} expansion with its normal "missing var"
// error, which is the right signal either way. Failures are printed to
// stderr so a genuinely missing secret isn't silently mysterious.
func loadProjectSecretsIntoEnv(deployFilePath string) {
	projectID, err := probeProjectID(deployFilePath)
	if err != nil || projectID == "" {
		return
	}

	cfg, _, err := config.Load()
	if err != nil {
		return
	}
	api, err := cfg.ResolveAPI()
	if err != nil {
		return
	}
	token := config.EffectiveAPIToken(api)
	if token == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy: project_id is set but not logged in (run `sklp auth login`) — skipping project secrets")
		return
	}

	cli := apiclient.New(api.URL, token)
	body, _, err := cli.Get(context.Background(), "/api/projects/"+projectID+"/secrets/resolve")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp deploy: fetch project secrets: %v\n", err)
		return
	}
	var resp struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "sklp deploy: parse project secrets response: %v\n", err)
		return
	}
	for k, v := range resp.Secrets {
		if _, set := os.LookupEnv(k); set {
			continue
		}
		os.Setenv(k, v)
	}
}

// probeProjectID reads only the project_id: field from a deploy.yaml,
// without running it through spacemanager.Parse's full validation — that
// validation resolves ${VAR} itself, which is exactly what hasn't happened
// yet at this point in the load sequence.
func probeProjectID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var probe struct {
		ProjectID string `yaml:"project_id"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return "", err
	}
	return probe.ProjectID, nil
}
