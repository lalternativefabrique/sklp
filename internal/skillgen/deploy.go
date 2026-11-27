package skillgen

import (
	"fmt"
	"strings"
)

func initDeploySkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: deploy contract\n\n")
	fmt.Fprintf(&b, "Deploy %s to a sklet node. `.sklp/<stack>/deploy.yaml` declares the services and jobs of one stack; the node runs them in a *space* (production, staging, a preview). `.sklp/deploy-root.yaml` says which manifest goes to which space on which node, for deploys driven by git.\n\n", v.ProjectName)
	b.WriteString(statusLine(v.HasDeployConfig, "writing `.sklp/<stack>/deploy.yaml` by hand from the shape below"))
	b.WriteString("\n\n## Two ways a stack reaches a node\n\n")
	b.WriteString("- **Explicit apply**: `sklp deploy apply <stack> -context <node>`. `-context` is mandatory, never the active context by default; print the target first (`sklp deploy context list`). The apply answers `created`, `configured`, `updated` or `unchanged` per space: `created` on a production node means the wrong context or space.\n")
	b.WriteString("- **Git reconcile**: with `.sklp/deploy-root.yaml`, a push to the default branch applies every listed manifest (core sends it to the node), or a node that follows the repo (`sklp deploy follow <https-repo>`) applies it itself. Other branches never reconcile; an empty commit retries.\n\n")
	b.WriteString("## deploy.yaml shape\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("version: 1\n")
	b.WriteString("kind: deploy\n")
	b.WriteString("name: app\n\n")
	b.WriteString("env_files: [.env]            # loaded before ${VAR} expansion; the host env wins\n\n")
	fmt.Fprintf(&b, "registries:                  # pull credentials for a private registry, keyed by host\n  %s:\n    username: nologin\n    password: ${REGISTRY_PASSWORD}\n\n", registryHost(v.Registry))
	b.WriteString("services:\n")
	b.WriteString("  api:\n")
	fmt.Fprintf(&b, "    image: %s/api:20261020-0912-ab12cd34   # immutable tag, never latest\n", v.Registry)
	b.WriteString("    port: 8080                 # the container port the space proxies to\n")
	b.WriteString("    env:\n")
	b.WriteString("      DATABASE_URL: postgres://app:${PG_PASSWORD}@postgres:5432/app\n")
	b.WriteString("    ready: { http: http://localhost:8080/health, timeout: 90s, soak: 60s }\n")
	b.WriteString("    depends_on: [postgres, migrate]\n")
	b.WriteString("    resources: { cpu: \"1\", memory: 512Mi, pids: 512 }\n")
	b.WriteString("  postgres:\n")
	b.WriteString("    image: postgres:17-bookworm\n")
	b.WriteString("    volumes: [\"pgdata:/var/lib/postgresql/data\"]   # named volumes only\n\n")
	b.WriteString("jobs:                         # run to completion; depends_on a job waits for its success\n")
	b.WriteString("  migrate:\n")
	fmt.Fprintf(&b, "    image: %s/migrate:20261020-0912-ab12cd34\n", v.Registry)
	b.WriteString("    depends_on: [postgres]\n\n")
	b.WriteString("spaces:\n")
	b.WriteString("  production:\n")
	b.WriteString("    api: { replicas: 2 }       # per-space overrides: replicas, port, proxy, env only\n")
	b.WriteString("    gateway:\n")
	b.WriteString("      routes:\n")
	b.WriteString("        - { service: api, host: app.example.com, kind: api }   # public TLS route\n")
	b.WriteString("```\n\n")
	b.WriteString("## Rules agents get wrong\n\n")
	b.WriteString("- Secrets are `${VAR}` references, never literals: values come from `env_files`, the stack config (`sklp config set -s <stack> --secret KEY value`) or the project's deploy secrets. The node keeps them in the space's 0600 env file, never in the manifest.\n")
	b.WriteString("- `volumes:` takes `name:/path[:ro|rw]` only: a host path (`/srv/x:/data`) is refused by the node.\n")
	b.WriteString("- `ready:` is one probe among `http`, `tcp`, `log`, `file`, plus `timeout` and `soak`. A rollout starts the new generation beside the old one and rolls back if it dies during the soak (60s by default).\n")
	b.WriteString("- Service names are unique across `services` and `jobs`; `depends_on` only names entries of the same file.\n")
	b.WriteString("- Another space is reached as `<service>.<space>.internal` on port 80, never by IP.\n")
	b.WriteString("- Per service, `gateway.routes` (host and path, with TLS) and `ingress` (raw host port, no TLS) are exclusive.\n")
	b.WriteString("- `capabilities:` beyond the baseline and images outside the node's trusted registries are refused by the node: that policy belongs to the node, not the manifest.\n")
	b.WriteString("- The loader ignores an unknown key without a word (`limits:` instead of `resources:` does nothing): check every key against the schema.\n\n")
	b.WriteString("## deploy-root.yaml shape\n\n")
	b.WriteString(statusLine(v.HasDeployRoot, "writing `.sklp/deploy-root.yaml` by hand from the shape below"))
	b.WriteString("\n\n```yaml\n")
	b.WriteString("kind: deploy-root\n")
	b.WriteString("node: <node-name>              # default target node\n")
	b.WriteString("deployments:\n")
	b.WriteString("  - space: app-production\n")
	b.WriteString("    path: .sklp/app/deploy.yaml  # repo-relative manifest path\n")
	b.WriteString("  - space: app-staging\n")
	b.WriteString("    path: .sklp/app/deploy.yaml\n")
	b.WriteString("    node: <other-node>         # per-entry override\n")
	b.WriteString("```\n\n")
	b.WriteString("Each entry needs `space` and `path`, and one space appears once. A repo without this file reconciles nothing.\n\n")
	b.WriteString("## Check before shipping\n\n")
	b.WriteString("With the sklp MCP server: `sklp_schema` (deploy, deploy-root) for the exact fields, then `contract_validate` until it reports no error. Its warnings name keys the node would silently ignore. The node's admission policy is checked only at apply, so read the apply's answer, then `sklp deploy get <space> -context <node>`.")
	return Skill{
		Name:        "init-deploy",
		Description: fmt.Sprintf("Write the deploy contract (.sklp/<stack>/deploy.yaml, .sklp/deploy-root.yaml) of %s and ship it to a node.", v.ProjectName),
		Body:        b.String(),
	}
}

func initWatchesSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Init: image watches\n\n")
	fmt.Fprintf(&b, "Roll %s's services when a new image is published. `.sklp/image-watches.yaml` tells a node to watch an image repository and roll one service of one space when the registry gains a tag that matches. Publishing is not deploying: the watch is what turns a pushed image into a running one.\n\n", v.ProjectName)
	b.WriteString(statusLine(v.HasImageWatches, "writing `.sklp/image-watches.yaml` by hand from the shape below"))
	b.WriteString("\n\n## Shape\n\n")
	b.WriteString("```yaml\n")
	b.WriteString("watches:\n")
	b.WriteString("  - name: app-api                # unique within the stack, across every repo feeding it\n")
	b.WriteString("    service: api                 # a service or job of the space's deploy.yaml\n")
	b.WriteString("    space: app-production        # required: a node hosts several spaces\n")
	b.WriteString("    node: <node-name>            # same form as deploy-root.yaml\n")
	fmt.Fprintf(&b, "    image_repo: %s/api   # no tag: the tag is what the watch discovers\n", v.Registry)
	b.WriteString("    # tag_pattern: '^[0-9]{8}-[0-9]{4}-[0-9a-f]+$'   # default: the publish tag YYYYMMDD-HHMM-<sha>\n")
	b.WriteString("    # registry_user: nologin     # private registry; its password is set on the stack, never here\n")
	b.WriteString("```\n\n")
	b.WriteString("## How a roll happens\n\n")
	b.WriteString("1. A push to the default branch syncs the node's watches with this file (a followed repo is read by the node itself). An entry removed from the file stops being watched.\n")
	b.WriteString("2. The publish pipeline pushes `<image_repo>:YYYYMMDD-HHMM-<sha>`; the watch sees the new tag and rolls the service.\n")
	b.WriteString("3. The new generation starts beside the old one, takes the traffic once ready and is kept through the soak; a crash rolls back.\n")
	b.WriteString("4. The node keeps the rolled tag across later applies of deploy.yaml until git changes that service's tag itself (three-way merge on the declared image): a watch never fights the manifest.\n\n")
	b.WriteString("## Rules agents get wrong\n\n")
	b.WriteString("- `image_repo` is the same repository as the service's `image:` in deploy.yaml, without the tag; a published tag that does not match `tag_pattern` never rolls.\n")
	b.WriteString("- `space` and `service` must exist: the space in deploy-root.yaml, the service in that space's deploy.yaml. A watch on a missing service rolls nothing.\n")
	b.WriteString("- Never write a registry password in the file.\n")
	b.WriteString("- `rolled` is acceptance, not success: check that the roll survived its soak before calling a release live.\n\n")
	b.WriteString("## Check\n\n")
	b.WriteString("With the sklp MCP server: `contract_validate` (image-watches) checks each entry and that its space and service exist; `deploy_watches` shows what the node actually watches, its current tag and its latest rolls.")
	return Skill{
		Name:        "init-watches",
		Description: fmt.Sprintf("Write .sklp/image-watches.yaml so a node rolls %s's services when a new image is published.", v.ProjectName),
		Body:        b.String(),
	}
}

func registryHost(registry string) string {
	if i := strings.Index(registry, "/"); i > 0 {
		return registry[:i]
	}
	return registry
}
