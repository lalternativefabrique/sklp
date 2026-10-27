package pipeline

import (
	"strings"
	"testing"
)

// env_files values must NOT reach a step that did not declare them.
//
// They used to be folded into Pipeline.Env, which CompileCI copies onto every
// step, so the whole dotenv — provider credentials included — was handed to
// every `run:` regardless of its `secrets:` list. A step running `pnpm install`
// exposed the full set to any postinstall script. These tests pin the opt-in.

func TestEnvFileSecretIsHiddenFromStepThatDidNotDeclareIt(t *testing.T) {
	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		repoFiles: map[string]string{
			".env": "SCW_SECRET_KEY=leaked-value-must-not-appear\n",
		},
		yaml: `version: 1
kind: pipeline
name: scoping

env_files:
  - .env

steps:
  - name: no-secrets-declared
    run: echo "seen=[${SCW_SECRET_KEY}]"
`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PassFail != "pass" {
		t.Fatalf("pipeline failed: %s", stdout)
	}
	if strings.Contains(stdout, "leaked-value-must-not-appear") {
		t.Fatalf("env_files secret leaked into a step that declared no secrets:\n%s", stdout)
	}
	if !strings.Contains(stdout, "seen=[]") {
		t.Fatalf("expected the variable to be empty in the step, got:\n%s", stdout)
	}
}

// The opt-in must still work: a step that declares the name receives the value,
// otherwise the hardening would simply break every pipeline that needs a secret.
func TestEnvFileSecretReachesStepThatDeclaresIt(t *testing.T) {
	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		repoFiles: map[string]string{
			".env": "MY_TOKEN=expected-value\n",
		},
		yaml: `version: 1
kind: pipeline
name: scoping-optin

env_files:
  - .env

secrets:
  - MY_TOKEN

steps:
  - name: declares-the-secret
    secrets: [MY_TOKEN]
    run: echo "seen=[${MY_TOKEN}]"
`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PassFail != "pass" {
		t.Fatalf("pipeline failed: %s", stdout)
	}
	if !strings.Contains(stdout, "seen=[expected-value]") {
		t.Fatalf("declared secret did not reach the step that opted in:\n%s", stdout)
	}
}

// Two steps in one pipeline: only the declaring one sees the value. This is the
// cross-step isolation that matters when a build step legitimately needs a token
// and a neighbouring `pnpm install` must not.
func TestEnvFileSecretIsScopedPerStep(t *testing.T) {
	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		repoFiles: map[string]string{
			".env": "SHARED_TOKEN=only-for-the-second-step\n",
		},
		yaml: `version: 1
kind: pipeline
name: scoping-per-step

env_files:
  - .env

secrets:
  - SHARED_TOKEN

steps:
  - name: untrusted-install
    run: echo "install-sees=[${SHARED_TOKEN}]"
  - name: trusted-build
    secrets: [SHARED_TOKEN]
    run: echo "build-sees=[${SHARED_TOKEN}]"
`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.PassFail != "pass" {
		t.Fatalf("pipeline failed: %s", stdout)
	}
	if !strings.Contains(stdout, "install-sees=[]") {
		t.Fatalf("the non-declaring step saw the secret:\n%s", stdout)
	}
	if !strings.Contains(stdout, "build-sees=[only-for-the-second-step]") {
		t.Fatalf("the declaring step did not receive the secret:\n%s", stdout)
	}
}
