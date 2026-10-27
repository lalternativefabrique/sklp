package skillgen

import (
	"fmt"
	"strings"
)

// OperatorCommands lists every sklp invocation the operator skill teaches.
// cmd/sklp checks each one against the real command tree, so the skill can
// never name a verb that does not exist.
var OperatorCommands = []string{
	"sklp whoami -json",
	"sklp project list -json",
	"sklp auth status",
	"sklp auth login",
	"sklp config ls -s <stack>",
	"sklp config get -s <stack> <key> --show",
	"sklp config set -s <stack> --secret <key> <value>",
	"sklp config passphrase status",
	"sklp secret list -project <id>",
	"sklp secret pull -project <id>",
	"sklp deploy context list",
	"sklp deploy context use <name>",
	"sklp deploy apply <stack> -context <name>",
	"sklp deploy ls",
	"sklp deploy logs <service> -space <space> -f",
	"sklp deploy exec <service> -space <space> -- <cmd>",
	"sklp deploy mesh ls",
	"sklp deploy datastores",
	"sklp deploy restores",
	"sklp flow start <name>",
	"sklp flow end",
	"sklp flow end --local",
	"sklp run ci",
	"sklp run publish -only <scopes>",
	"sklp dev",
	"sklp dev --validate",
	"sklp issue new <title> --type bug --body <text>",
	"sklp cache status",
	"sklp clean --dry-run",
}

func operatorSkill(v Vars) Skill {
	var b strings.Builder
	fmt.Fprintf(&b, "# sklp operator: act without asking\n\n")
	fmt.Fprintf(&b, "In %s, `sklp` is the operator's interface to the Skalpai API, the repo's `.sklp/` contracts, the deploy nodes and their secrets. Everything below is discoverable from the shell; never ask the user for an id, a context name or a key that a command can print.\n\n", v.ProjectName)

	b.WriteString("## First command of any task\n\n")
	b.WriteString("```bash\nsklp whoami -json\n```\n\n")
	b.WriteString("One JSON document: `api` (url, logged_in, email), `projects` (id, name, stack_id, github_repo), `contexts` (name, server, space, current), `repo.stacks` (name, contracts, config keys with `secret: true/false`), `repo.pipelines`, `secrets_passphrase_source`. Read ids from it. If `api.logged_in` is false, run `sklp auth login` once.\n\n")

	b.WriteString("## I need X → I run Y\n\n")
	b.WriteString("| Need | Command | Notes |\n|---|---|---|\n")
	b.WriteString("| a project id | `sklp whoami -json` or `sklp project list -json` | `-project <id>` and `SKLP_PROJECT` take that id |\n")
	b.WriteString("| a stack id | same: `projects[].stack_id` | one stack groups several projects |\n")
	b.WriteString("| the API login | `sklp auth status` | `sklp auth login` saves a token in `~/.config/sklp/config.yaml` |\n")
	b.WriteString("| which config keys a stack has | `sklp config ls -s <stack>` | secrets print as `<secret>`; the file is `.sklp/<stack>/config.yaml`, committable |\n")
	b.WriteString("| a secret value | `sklp config get -s <stack> <key> --show` | needs the passphrase: `SKLP_SECRETS_PASSPHRASE`, a configured command, or the OS vault (`sklp config passphrase status`). Never paste a value into a file or a commit |\n")
	b.WriteString("| set a secret | `sklp config set -s <stack> --secret <key> <value>` | prefer `--secret` for anything that is not public |\n")
	b.WriteString("| project secrets held by core | `sklp secret list -project <id>` / `sklp secret pull -project <id>` | deploy-readable ones only; `list` never prints plaintext |\n")
	b.WriteString("| where a deploy goes | `sklp deploy context list` | `*` marks the active context; every `deploy` verb targets it unless `-context` is given |\n")
	b.WriteString("| change the target | `sklp deploy context use <name>` | do this explicitly before a production apply and check the printed target |\n")
	b.WriteString("| apply a stack to a node | `sklp deploy apply <stack> -context <name>` | the output says `created` / `configured` / `updated`; `created` on a production node means the wrong context |\n")
	b.WriteString("| what runs on a node | `sklp deploy ls` / `sklp deploy datastores` / `sklp deploy restores` | node-scoped, read-only |\n")
	b.WriteString("| logs of a service | `sklp deploy logs <service> -space <space> -f` | read-only |\n")
	b.WriteString("| a shell in a service | `sklp deploy exec <service> -space <space> -- <cmd>` | |\n")
	b.WriteString("| the WireGuard mesh | `sklp deploy mesh ls` | peers, mesh IPs, sign keys |\n")
	b.WriteString("| the local stack | `sklp dev` (`sklp dev --validate` parses only) | contract: `.sklp/<stack>/dev.yaml` |\n")
	b.WriteString("| a bug to file | `sklp issue new <title> --type bug --body <text>` | |\n")
	b.WriteString("| cache size | `sklp cache status` / `sklp clean --dry-run` | see rules |\n\n")

	b.WriteString("## Change lifecycle\n\n")
	b.WriteString("```bash\nsklp flow start <name>     # worktree + branch feat/<name>; never work on main\n# ...edit, test...\nsklp flow end              # diff vs main, push, open the PR; CI runs on the PR\nsklp flow end --local      # run the pipeline here first, push nothing\n```\n\n")
	b.WriteString("Merge on the remote (`gh pr merge`), then `git pull`. Publish images only from a clean `main`: `sklp run publish -only <scopes>`. `sklp run ci` runs the CI pipeline locally.\n\n")

	b.WriteString("## Rules that are not negotiable\n\n")
	b.WriteString("- Never delete anything under `~/.cache/sklp/` by hand; `sklp clean` (ask first) is the only way.\n")
	b.WriteString("- Never stop a stack with `pkill`/`kill` by pattern; Ctrl-C or the sklp verb that started it.\n")
	b.WriteString("- Never print, log or commit a secret value; pass ids and keys, not values.\n")
	b.WriteString("- Never push `main` or merge a PR unless the user asked for that action.\n")
	b.WriteString("- No GitHub Actions: pipelines run through sklp, dispatched by the Skalpai app or locally.\n")
	b.WriteString("- Before a production `deploy apply`, print the target (`sklp deploy context list`) and say it.\n\n")

	b.WriteString("## Reference\n\n")
	b.WriteString("`sklp <verb> --help` documents every flag; the tree is also served by the docs site. Contracts: `.sklp/<stack>/{deploy,dev,config}.yaml`, `.sklp/pipelines/*.yaml`, `.sklp/deploy-root.yaml`, `.sklp/image-watches.yaml`, `.sklp/space.yaml`.")

	return Skill{
		Name:        "sklp-operator",
		Description: fmt.Sprintf("Operate %s through sklp without asking: whoami, project and stack ids, secrets, deploy contexts, nodes, the change lifecycle and the hard rules.", v.ProjectName),
		Body:        b.String(),
	}
}
