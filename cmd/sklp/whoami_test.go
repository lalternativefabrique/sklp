package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/skillgen"
)

func TestWhoamiReportsRepoStacksWithoutPrintingSecrets(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SKLP_SECRETS_PASSPHRASE", "pw")
	repo := t.TempDir()
	stack := filepath.Join(repo, ".sklp", "prod")
	if err := os.MkdirAll(stack, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"deploy.yaml", "dev.yaml"} {
		if err := os.WriteFile(filepath.Join(stack, f), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "config:\n  DB_HOST: db\n  DB_PASSWORD:\n    secure: sealed\n"
	if err := os.WriteFile(filepath.Join(stack, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := buildWhoami(context.Background(), repo)
	if rep.API.LoggedIn {
		t.Fatal("no config: must not report a login")
	}
	if len(rep.Repo.Stacks) != 1 || rep.Repo.Stacks[0].Name != "prod" {
		t.Fatalf("stacks = %+v", rep.Repo.Stacks)
	}
	st := rep.Repo.Stacks[0]
	if strings.Join(st.Contracts, ",") != "deploy,dev" {
		t.Fatalf("contracts = %v", st.Contracts)
	}
	keys := map[string]bool{}
	for _, k := range st.Config {
		keys[k.Name] = k.Secret
	}
	if keys["DB_HOST"] || !keys["DB_PASSWORD"] {
		t.Fatalf("config keys = %+v", st.Config)
	}
	if rep.Passphrase != "env" {
		t.Fatalf("passphrase source = %q", rep.Passphrase)
	}

	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("sealed")) {
		t.Fatal("the report leaks a secret value")
	}
	var text bytes.Buffer
	renderWhoami(&text, rep)
	if !strings.Contains(text.String(), "DB_PASSWORD🔒") {
		t.Fatalf("text form does not mark the secret:\n%s", text.String())
	}
}

// Every command the operator skill teaches must exist in the real tree,
// or the skill drifts from the CLI it describes.
func TestOperatorSkillCommandsExist(t *testing.T) {
	root := rootCommand()
	for _, line := range skillgen.OperatorCommands {
		words := strings.Fields(line)
		if words[0] != "sklp" {
			t.Fatalf("%q does not start with sklp", line)
		}
		cmd := root
		path := "sklp"
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-") || strings.HasPrefix(w, "<") || cmd.RawArgs || len(cmd.Positionals) > 0 {
				break
			}
			next := findSub(cmd, w)
			if next == nil {
				t.Errorf("%q: %s has no sub-verb %q", line, path, w)
				break
			}
			cmd = *next
			path += " " + w
		}
	}
}

func findSub(cmd cli.Command, name string) *cli.Command {
	for i := range cmd.Sub {
		if cmd.Sub[i].Name == name {
			return &cmd.Sub[i]
		}
		for _, a := range cmd.Sub[i].Aliases {
			if a == name {
				return &cmd.Sub[i]
			}
		}
	}
	return nil
}
