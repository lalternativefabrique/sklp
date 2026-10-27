package skillgen

import (
	"fmt"
	"strings"
)

// lifecycle.go holds one skill per stage of the sklp lifecycle:
// init-dev, init-ci, init-config, init-publish. Each teaches the
// assistant which sklp verb owns that stage, where the contract lives,
// and whether it already exists in this repo (so the skill says "run it"
// vs "generate it first"). These are the vision made operational: an
// assistant primed on these can drive the repo from dev → CI → publish
// without the developer re-explaining the toolchain each time.

// statusLine renders "✓ already present" / "✗ not yet generated" for a
// contract, so the lifecycle skill tells the assistant the current state
// of the repo rather than generic advice.
func statusLine(exists bool, genCmd string) string {
	if exists {
		return "This contract **already exists** in the repo — run it, don't regenerate it."
	}
	return fmt.Sprintf("This contract is **not yet present** — generate it first with `%s`, review the draft, then use it.", genCmd)
}

func initDevSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: local dev stack\n\n")
	fmt.Fprintf(&b, "Stand up the local dev stack for %s. `sklp dev stack` supervises the services declared in `.sklp/stack/dev.yaml` (long-running processes with readiness + dependency ordering).\n\n", v.ProjectName)
	b.WriteString(statusLine(v.HasDevConfig, "writing `.sklp/stack/dev.yaml` by hand from the shape below"))
	b.WriteString("\n\n## Commands\n\n")
	b.WriteString("```bash\n")
	b.WriteString("# You author .sklp/stack/dev.yaml directly (see the file shape below), then:\n")
	b.WriteString("sklp dev stack --validate  # parse-only sanity check, no launch\n")
	b.WriteString("sklp dev stack             # boot the stack\n")
	b.WriteString("sklp dev down              # stop a stack started in another terminal\n")
	b.WriteString("```\n\n")
	b.WriteString("A stack is a folder: `.sklp/<stack>/dev.yaml`. A mono-app uses the default name `stack`; split into multiple folders only if the repo has truly independent stacks. Keep entries to `run` / `ready` / `depends_on`. Optional services are opt-in via a flag in the file.\n\n")

	// Authoring contract. The top three lines are MANDATORY and the most
	// common omission when an agent writes this file from scratch — a
	// missing `kind:` makes `sklp dev stack --validate` fail with
	// `kind must be "devstack", got ""`.
	b.WriteString("## File shape (write this exactly)\n\n")
	b.WriteString("Every dev.yaml MUST start with these three lines — `version`, `kind`, `name`:\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("version: 1\n")
	b.WriteString("kind: devstack          # REQUIRED — omitting it fails validation\n")
	b.WriteString("name: dev\n")
	b.WriteString("env_files:\n")
	b.WriteString("  - .env                # wire every env template the apps read (also apps/*/.env.example, .env-template)\n")
	b.WriteString("services:\n")
	b.WriteString("  postgres:             # a DB service example — drop if the repo has no database\n")
	b.WriteString("    image: postgres:16-alpine\n")
	b.WriteString("    ports: [\"5432:5432\"]\n")
	b.WriteString("    env: { POSTGRES_PASSWORD: postgres }\n")
	b.WriteString("    ready:\n")
	b.WriteString("      tcp: localhost:5432    # for a native image: DB, tcp waits for the real server\n")
	b.WriteString("      timeout: 60s           # (use ready.log \"...ready to accept connections\" for a DB started via docker compose)\n")
	b.WriteString("  migrate:\n")
	b.WriteString("    run: <migrate cmd>     # e.g. pnpm db:migrate / drizzle-kit migrate — only if the repo has migrations\n")
	b.WriteString("    depends_on: [postgres]\n")
	b.WriteString("    oneshot: true          # runs to exit 0, then unblocks dependents; never give a oneshot a ready: block\n")
	b.WriteString("  web:\n")
	b.WriteString("    run: <dev cmd> --host 0.0.0.0 --port <p>  # bind 0.0.0.0, NOT localhost (vite/next default to loopback → unreachable from the host)\n")
	b.WriteString("    ports: [\"<p>:<p>\"]   # publish so the browser opens http://localhost:<p>\n")
	b.WriteString("    depends_on: [migrate]\n")
	b.WriteString("```\n\n")
	b.WriteString("A `run:` web service MUST bind `0.0.0.0` and declare `ports:` to be reachable from the host browser — each service has its own netns reached by bridge IP, so a localhost-only bind never goes ready. ")
	b.WriteString("Validate with `sklp dev stack --validate` BEFORE finishing — it catches a missing `kind:`, bad `depends_on`, and probe typos. Do not invent services the repo has no evidence for.")
	return Skill{
		Name:        "init-dev",
		Description: fmt.Sprintf("Generate and run the local dev stack (.sklp/stack/dev.yaml) for %s.", v.ProjectName),
		Body:        b.String(),
	}
}

func initCISkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: CI contract\n\n")
	fmt.Fprintf(&b, "Verify %s before pushing. `sklp run ci` executes the `.sklp/pipelines/ci.yaml` pipeline locally (or on a self-hosted runner) — the same contract everywhere, no GitHub Actions.\n\n", v.ProjectName)
	b.WriteString(statusLine(v.HasCITask, "writing `.sklp/pipelines/ci.yaml` by hand (version 1, kind: pipeline)"))
	b.WriteString("\n\n## Commands\n\n")
	b.WriteString("```bash\n")
	b.WriteString("# You author .sklp/pipelines/ci.yaml directly (version: 1 / kind: pipeline / tools / scopes / steps), then:\n")
	b.WriteString("sklp run ci                # run the pipeline\n")
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "The generated pipeline is stack-aware (%s): lint, test, and a security audit step per language when a lockfile is present. Run it before every push as the pre-flight gate.\n\n", v.langList())

	// Secrets — the #1 thing an agent gets wrong when authoring ci.yaml.
	b.WriteString("## Secrets (env vars a step needs at run time)\n\n")
	b.WriteString("A secret is an env var (an API token, a registry password, a DB url) the pipeline needs but must NOT be committed. The local source is a gitignored dotenv (`.env.local`); the shell env wins over it when both define the same key. A secret name MUST match `[A-Z_][A-Z0-9_]*`.\n\n")
	b.WriteString("Wiring is a **two-level opt-in** — declare the secret at BOTH levels or the step sees an empty value:\n\n")
	b.WriteString("1. pipeline-level `secrets:` — resolves the value (fails the run early if it is missing), and\n")
	b.WriteString("2. the consuming step's own `secrets:` — opts that one step into seeing it (least privilege; a step without it never receives the value).\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("env_files:\n")
	b.WriteString("  - .env.local            # gitignored; KEY=value per line. Missing file is fine.\n")
	b.WriteString("secrets:\n")
	b.WriteString("  - MY_TOKEN              # pipeline-level: resolved from env / .env.local, fail-fast if unset\n")
	b.WriteString("steps:\n")
	b.WriteString("  - name: deploy\n")
	b.WriteString("    secrets: [MY_TOKEN]   # step-level opt-in — REQUIRED, else $MY_TOKEN is empty in the run\n")
	b.WriteString("    run: ./deploy.sh      # $MY_TOKEN is now in the step env\n")
	b.WriteString("```\n\n")
	b.WriteString("```bash\n")
	b.WriteString("# .env.local (NEVER commit — add it to .gitignore)\n")
	b.WriteString("MY_TOKEN=the-value\n")
	b.WriteString("# or pass it inline for one run (shell wins over .env.local):\n")
	b.WriteString("MY_TOKEN=the-value sklp run ci\n")
	b.WriteString("```\n\n")
	b.WriteString("Do NOT special-case `GITHUB_TOKEN` for cloning private repos in a step: `sklp run` already wires git auth (an `insteadOf` rewrite for both HTTPS and SSH github.com URLs) from `GITHUB_TOKEN` in the env / `.env.local`, so `go mod download` / `git clone` of private modules just work. You still declare it under `secrets:` only when a step reads the token's VALUE itself (e.g. `gh api`, `npm publish`).")
	return Skill{
		Name:        "init-ci",
		Description: fmt.Sprintf("Generate and run the CI pipeline (.sklp/pipelines/ci.yaml) for %s.", v.ProjectName),
		Body:        b.String(),
	}
}

func initConfigSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: runner config\n\n")
	fmt.Fprintf(&b, "Define the runner image environment for %s. `.sklp/space.yaml` pins the toolchain versions, apt packages, and registry the runner image bakes — it is the definition of the closed environment (the *space*) the project runs in. Generation is **deterministic** (no LLM) — the image is a build cache keyed by the recipe hash, so output is reproducible.\n\n", v.ProjectName)
	b.WriteString("## Commands\n\n")
	b.WriteString("```bash\n")
	b.WriteString("sklp init -only config      # deterministic scan → .sklp/space.yaml\n")
	b.WriteString("sklp runner build           # build the runner image from it\n")
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "Toolchain versions come from the repo's pin files (go.mod, .nvmrc, .tool-versions, rust-toolchain.toml, …). Registry root: `%s`. Bumping a pin must be followed by a runner rebuild or CI uses the stale toolchain.", v.Registry)
	return Skill{
		Name:        "init-config",
		Description: fmt.Sprintf("Generate the deterministic space recipe (.sklp/space.yaml) for %s.", v.ProjectName),
		Body:        b.String(),
	}
}

func initPublishSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: publish contract\n\n")
	fmt.Fprintf(&b, "Ship %s safely. `sklp run publish` executes `.sklp/pipelines/publish.yaml`: detect impacted services, build → Trivy scan → push. Guard-rails refuse a dirty tree, HIGH/CRITICAL vulns, or a non-`main` branch.\n\n", v.ProjectName)
	b.WriteString(statusLine(v.HasPublishTask, "sklp init -only publish"))
	b.WriteString("\n\n## Commands\n\n")
	b.WriteString("```bash\n")
	b.WriteString("sklp init -only publish     # generate a draft .sklp/pipelines/publish.yaml.draft\n")
	b.WriteString("sklp run publish -dry-run   # validate the contract without pushing\n")
	b.WriteString("sklp run publish            # publish impacted services (main only)\n")
	b.WriteString("```\n\n")
	fmt.Fprintf(&b, "Registry root: `%s`. Image tags are immutable `YYYYMMDD-HHMM-<sha>`. **Never publish from a feature branch.** Pushing an image is not deploying — don't claim deployment success when only the image was pushed; rollout is handled downstream (ArgoCD / image updater).", v.Registry)
	return Skill{
		Name:        "init-publish",
		Description: fmt.Sprintf("Generate and run the publish pipeline (.sklp/pipelines/publish.yaml) for %s.", v.ProjectName),
		Body:        b.String(),
	}
}
