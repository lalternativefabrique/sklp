package remoteapply

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// LoadProjectSecrets sets in the process env every deploy-readable secret
// core holds for the deploy file's project_id, leaving set names untouched.
// Best-effort: a value missing here fails later as an unset ${VAR}.
func LoadProjectSecrets(deployFilePath string, warn io.Writer) {
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
		fmt.Fprintln(warn, "sklp deploy: project_id is set but not logged in (run `sklp auth login`) — skipping project secrets")
		return
	}

	cli := apiclient.New(api.URL, token)
	body, _, err := cli.Get(context.Background(), "/api/projects/"+projectID+"/secrets/resolve")
	if err != nil {
		fmt.Fprintf(warn, "sklp deploy: fetch project secrets: %v\n", err)
		return
	}
	var resp struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(warn, "sklp deploy: parse project secrets response: %v\n", err)
		return
	}
	for k, v := range resp.Secrets {
		if _, set := os.LookupEnv(k); set {
			continue
		}
		os.Setenv(k, v)
	}
}

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
