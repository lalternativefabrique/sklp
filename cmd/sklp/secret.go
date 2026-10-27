// secret.go implements `sklp secret {push,pull,list,delete}`. It syncs
// project_secret entries in core with a local dotenv file, so a developer
// can populate/refresh project secrets from the terminal instead of the
// web UI, and bootstrap a fresh clone's secrets file from what's stored.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

const defaultSecretFile = ".env.secret"

// secretItem mirrors project_secret.SecretResponse. Never carries plaintext.
type secretItem struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	Name           string `json:"name"`
	ValueLast4     string `json:"value_last4"`
	DeployReadable bool   `json:"deploy_readable"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

func secretCommand() cli.Command {
	run := func(fn func(*issueIO, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultIssueIO(), args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp secret:", err)
				return 1
			}
			return 0
		}
	}
	filePos := []cli.Positional{{Name: "file"}}
	return cli.Command{
		Name:  "secret",
		Short: "Sync project secrets between core and a local dotenv file (push | pull | list | delete)",
		Sub: []cli.Command{
			{Name: "push", Short: "Upload dotenv KEY=VALUE pairs to core", Flags: secretPushFlags(), Positionals: filePos, Run: run(secretPush)},
			{Name: "pull", Short: "Fetch deploy-readable secrets into a local dotenv file", Flags: secretPullFlags(), Positionals: filePos, Run: run(secretPull)},
			{Name: "list", Short: "List project secrets (name, last4, deploy-readable — never plaintext)", Flags: secretListFlags(), Run: run(secretList)},
			{Name: "delete", Short: "Delete a project secret by name", Flags: []cli.Flag{issueProjectFlag}, Positionals: []cli.Positional{{Name: "name", Required: true}}, Run: run(secretDelete)},
		},
	}
}

func secretPushFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "deploy-readable", Type: cli.BoolFlag, Usage: "opt the secret into `sklp deploy`/`secret pull` (default true for new secrets)", BoolDefault: true},
		{Name: "dry-run", Type: cli.BoolFlag, Usage: "print what would change, upload nothing"},
	}
}

func secretPullFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "force", Type: cli.BoolFlag, Usage: "skip the confirmation prompt when pulling into a non-default filename"},
	}
}

func secretListFlags() []cli.Flag {
	return []cli.Flag{issueProjectFlag, issueJSONFlag, {Name: "plain", Type: cli.BoolFlag, Usage: "emit TSV (one row per secret, no header)"}}
}

func secretFilePath(args cli.Args) string {
	if f := firstArg(args, 0); f != "" {
		return f
	}
	return defaultSecretFile
}

func secretPush(iio *issueIO, args cli.Args) error {
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	file := secretFilePath(args)
	local, err := parseDotenvValues(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	if len(local) == 0 {
		return fmt.Errorf("%s has no KEY=VALUE entries", file)
	}
	names := make([]string, 0, len(local))
	for k := range local {
		if err := validateSecretName(k); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		names = append(names, k)
	}
	sort.Strings(names)

	deployReadable := args.Bool("deploy-readable")

	apic, err := resolveClient()
	if err != nil {
		return err
	}
	dryRun := args.Bool("dry-run")

	existing, err := fetchSecrets(apic, project)
	if err != nil {
		return err
	}
	byName := make(map[string]secretItem, len(existing))
	for _, s := range existing {
		byName[s.Name] = s
	}

	var failures []string
	for _, name := range names {
		value := local[name]
		if sec, ok := byName[name]; ok {
			if dryRun {
				fmt.Fprintf(iio.Stdout, "update %s\n", name)
				continue
			}
			if err := patchSecretValue(apic, project, sec.ID, value, deployReadable, args.IsSet("deploy-readable")); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			fmt.Fprintf(iio.Stdout, "%s: updated\n", name)
			continue
		}

		if dryRun {
			fmt.Fprintf(iio.Stdout, "create %s\n", name)
			continue
		}
		payload := map[string]any{"name": name, "value": value, "deploy_readable": deployReadable}
		_, status, err := apic.Post(context.Background(), "/api/projects/"+url.PathEscape(project)+"/secrets", payload)
		if err != nil {
			if isConflict(err, status) {
				id, lerr := lookupSecretID(apic, project, name)
				if lerr != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", name, lerr))
					continue
				}
				if err := patchSecretValue(apic, project, id, value, false, false); err != nil {
					failures = append(failures, fmt.Sprintf("%s: %v", name, err))
					continue
				}
				fmt.Fprintf(iio.Stdout, "%s: updated\n", name)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", name, wrapAPIError(err, status)))
			continue
		}
		fmt.Fprintf(iio.Stdout, "%s: created\n", name)
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d secret(s) failed to push:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return nil
}

// patchSecretValue issues the PATCH for an existing secret. deployReadable is
// only sent when set is true, matching UpdateRequest's pointer semantics:
// omitting the field leaves DeployReadable untouched server-side.
func patchSecretValue(apic *apiclient.Client, project, secretID, value string, deployReadable, set bool) error {
	payload := map[string]any{"value": value}
	if set {
		payload["deploy_readable"] = deployReadable
	}
	path := "/api/projects/" + url.PathEscape(project) + "/secrets/" + url.PathEscape(secretID)
	if _, status, err := apic.Patch(context.Background(), path, payload); err != nil {
		return wrapAPIError(err, status)
	}
	return nil
}

func lookupSecretID(apic *apiclient.Client, project, name string) (string, error) {
	existing, err := fetchSecrets(apic, project)
	if err != nil {
		return "", err
	}
	for _, s := range existing {
		if s.Name == name {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("secret %s not found", name)
}

func isConflict(err error, status int) bool {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusConflict
	}
	return status == http.StatusConflict
}

func secretPull(iio *issueIO, args cli.Args) error {
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	file := secretFilePath(args)
	if filepath.Base(file) != defaultSecretFile && !args.Bool("force") {
		return fmt.Errorf("pulling into %s (not the default %s) requires --force", file, defaultSecretFile)
	}

	apic, err := resolveClient()
	if err != nil {
		return err
	}
	body, status, err := apic.Get(context.Background(), "/api/projects/"+url.PathEscape(project)+"/secrets/resolve")
	if err != nil {
		return wrapAPIError(err, status)
	}
	var resp struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}

	lines, err := readDotenvLines(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	updated, added := mergeDotenvLines(&lines, resp.Secrets)

	if err := writeDotenvLines(file, lines); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	fmt.Fprintf(iio.Stdout, "pulled %d secret(s) into %s (%d updated, %d added)\n", len(resp.Secrets), file, updated, added)
	return nil
}

func secretList(iio *issueIO, args cli.Args) error {
	if args.Bool("json") && args.Bool("plain") {
		return errors.New("--json and --plain are mutually exclusive")
	}
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	apic, err := resolveClient()
	if err != nil {
		return err
	}
	body, status, err := apic.Get(context.Background(), "/api/projects/"+url.PathEscape(project)+"/secrets")
	if err != nil {
		return wrapAPIError(err, status)
	}

	if args.Bool("json") {
		_, werr := iio.Stdout.Write(body)
		if werr != nil {
			return werr
		}
		if len(body) > 0 && body[len(body)-1] != '\n' {
			fmt.Fprintln(iio.Stdout)
		}
		return nil
	}

	var items []secretItem
	if err := json.Unmarshal(body, &items); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if args.Bool("plain") {
		return renderSecretsPlain(iio.Stdout, items)
	}
	return renderSecretsTable(iio.Stdout, items)
}

func secretDelete(iio *issueIO, args cli.Args) error {
	name := firstArg(args, 0)
	if name == "" {
		return errors.New("usage: sklp secret delete <name> [flags]")
	}
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	apic, err := resolveClient()
	if err != nil {
		return err
	}
	id, err := lookupSecretID(apic, project, name)
	if err != nil {
		return fmt.Errorf("secret %s not found in project %s", name, project)
	}

	if _, status, err := apic.Delete(context.Background(), "/api/projects/"+url.PathEscape(project)+"/secrets/"+url.PathEscape(id)); err != nil {
		return wrapAPIError(err, status)
	}
	fmt.Fprintf(iio.Stdout, "deleted secret %s\n", name)
	return nil
}

func fetchSecrets(apic *apiclient.Client, project string) ([]secretItem, error) {
	body, status, err := apic.Get(context.Background(), "/api/projects/"+url.PathEscape(project)+"/secrets")
	if err != nil {
		return nil, wrapAPIError(err, status)
	}
	var items []secretItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return items, nil
}

func renderSecretsTable(w io.Writer, items []secretItem) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tLAST4\tDEPLOY_READABLE\tUPDATED")
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n",
			it.Name, emptyDash(it.ValueLast4), it.DeployReadable, it.UpdatedAt)
	}
	return tw.Flush()
}

func renderSecretsPlain(w io.Writer, items []secretItem) error {
	for _, it := range items {
		fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", it.Name, it.ValueLast4, it.DeployReadable, it.UpdatedAt)
	}
	return nil
}

func validateSecretName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("%s: name must be 64 characters or less", name)
	}
	for i, c := range name {
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		isUnderscore := c == '_'
		if i == 0 && !(isUpper || isUnderscore) {
			return fmt.Errorf("%s: name must start with an uppercase letter or underscore", name)
		}
		if !(isUpper || isDigit || isUnderscore) {
			return fmt.Errorf("%s: name must contain only A-Z, 0-9, and underscores", name)
		}
	}
	return nil
}
