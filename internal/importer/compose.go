package importer

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/repofacts"
)

type ComposeOptions struct {
	Root      string
	Stack     string
	Runnables []repofacts.RunnableFacts
}

// ComposeImport is a `.sklp/<stack>/dev.yaml` translated from the compose
// files `docker compose up` would read, with one Note per translation choice
// and per action left to the developer.
type ComposeImport struct {
	Sources []string
	YAML    []byte
	Notes   []Note
}

// ImportCompose returns nil when root holds no compose file.
func ImportCompose(opts ComposeOptions) (*ComposeImport, error) {
	files := FindComposeFiles(opts.Root)
	if len(files) == 0 {
		return nil, nil
	}
	proj, err := loadComposeProject(opts.Root, files)
	if err != nil {
		return nil, err
	}
	proj.resolveExtends()
	t := &translator{opts: opts, proj: proj, vars: loadVarSource(opts.Root), stack: &devStack{}}
	if t.opts.Stack == "" {
		t.opts.Stack = "<stack>"
	}
	t.translate()
	return &ComposeImport{Sources: files, YAML: renderDevStack(t.stack, files), Notes: t.stack.allNotes()}, nil
}

type devStack struct {
	envFiles  []string
	allowHost bool
	services  []*devService
	notes     []Note
}

func (s *devStack) find(name string) *devService {
	for _, svc := range s.services {
		if svc.name == name {
			return svc
		}
	}
	return nil
}

func (s *devStack) allNotes() []Note {
	out := append([]Note(nil), s.notes...)
	for _, svc := range s.services {
		out = append(out, svc.notes...)
	}
	return out
}

type devReady struct {
	http, tcp, log, timeout string
}

type devService struct {
	name      string
	image     string
	run       string
	workdir   string
	port      int
	command   []string
	env       map[string]string
	volumes   []string
	dependsOn []string
	oneshot   bool
	optional  bool
	ready     *devReady
	network   string
	fromBuild bool
	detected  bool

	composeWorkdir string
	servedBy       string
	devServerRun   bool

	healthProbe  *devReady
	needsHealthy bool
	notes        []Note
}

func (s *devService) note(sev Severity, field, format string, args ...any) {
	s.notes = append(s.notes, Note{Severity: sev, Service: s.name, Field: field, Message: fmt.Sprintf(format, args...)})
}

type translator struct {
	opts  ComposeOptions
	proj  *composeProject
	vars  *varSource
	stack *devStack

	delegatesToDotenv bool
	namedVolumes      []string
}

func (t *translator) stackNote(sev Severity, field, format string, args ...any) {
	t.stack.notes = append(t.stack.notes, Note{Severity: sev, Field: field, Message: fmt.Sprintf(format, args...)})
}

func (t *translator) translate() {
	for _, cs := range t.proj.services {
		t.stack.services = append(t.stack.services, t.service(cs))
	}
	for _, cs := range t.proj.services {
		t.dependsOn(cs)
	}
	t.readiness()
	t.uncoveredRunnables()
	t.topLevel()
	if t.delegatesToDotenv {
		t.stack.envFiles = appendUniqueString([]string{".env"}, t.stack.envFiles...)
	}
	if len(t.stack.envFiles) > 0 {
		t.stackNote(Info, "env_files", "sklp dev loads %s into every service of the stack, whereas compose scoped env_file: to one service and read .env only to interpolate the compose file", strings.Join(t.stack.envFiles, ", "))
	}
	if len(t.namedVolumes) > 0 {
		t.stackNote(Info, "volumes", "named volumes %s start empty under sklp: data in the existing docker volumes is not copied (dump/restore it if you need it)", strings.Join(uniqueSorted(t.namedVolumes), ", "))
	}
}

var droppedFields = map[string]string{
	"container_name":    "the service name is the only hostname in sklp dev",
	"restart":           "sklp dev does not restart a crashed service; the stack reports it",
	"labels":            "no labels in sklp dev",
	"logging":           "logs are streamed by sklp dev, prefixed by service",
	"stdin_open":        "sklp dev attaches no stdin to a service",
	"tty":               "no TTY allocation",
	"init":              "sklp runs its own init as PID 1",
	"stop_grace_period": "sklp dev sends SIGTERM then SIGKILL after its own grace period",
	"stop_signal":       "sklp dev always stops with SIGTERM",
	"pull_policy":       "images are pulled once and cached",
	"hostname":          "the service name is the hostname",
	"domainname":        "the service name is the hostname",
	"read_only":         "service rootfs is already read-only; declare writable paths as volumes",
	"develop":           "the repo is bind-mounted live; reloading is the dev server's job",
	"platform":          "images are pulled for the host architecture",
	"mem_limit":         "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"memswap_limit":     "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"mem_reservation":   "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"cpus":              "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"cpu_shares":        "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"cpuset":            "dev.yaml has no resource limits (deploy.yaml has `resources:`)",
	"deploy":            "dev.yaml has no replicas or resource limits (deploy.yaml has `resources:`)",
	"scale":             "sklp dev runs one instance per service",
}

var manualFields = map[string]string{
	"user":             "sklp runs the container as the image's user inside a rootless user namespace; there is no `user:` override — bake the user into the image or drop the need",
	"privileged":       "privileged containers are refused by sklp; find which capability the service really needs and remove the dependency",
	"cap_add":          "added capabilities are refused by sklp's admission policy; the service must run with the default set",
	"cap_drop":         "sklp already runs with a reduced capability set; nothing to carry over, but check the service does not rely on the drop",
	"devices":          "host devices cannot be mounted into a sklp space",
	"dns":              "spaces use sklp's resolver; custom DNS servers are not configurable",
	"dns_search":       "spaces use sklp's resolver; search domains are not configurable",
	"dns_opt":          "spaces use sklp's resolver; resolver options are not configurable",
	"tmpfs":            "no tmpfs mounts: declare a named volume on that path instead (e.g. `<svc>-tmp:/tmp`)",
	"shm_size":         "/dev/shm size is not configurable; check the service (Chrome, Postgres parallel workers) works with the default",
	"ulimits":          "ulimits are not configurable per service",
	"sysctls":          "sysctls are not configurable per service",
	"secrets":          "compose secrets are files under /run/secrets; sklp dev has none — pass the value through env (from .env) or a volume",
	"configs":          "compose configs are not supported: bake the file into the image (`COPY`) or pass the setting through env",
	"security_opt":     "security options are not configurable",
	"pid":              "shared PID namespaces are not supported",
	"ipc":              "shared IPC namespaces are not supported",
	"gpus":             "GPU passthrough is not supported",
	"userns_mode":      "sklp always runs services in its own user namespace",
	"cgroup_parent":    "cgroups are managed by sklp",
	"runtime":          "the OCI runtime is sklp's embedded runc",
	"isolation":        "isolation mode is not configurable",
	"storage_opt":      "storage options are not configurable",
	"oom_kill_disable": "OOM settings are not configurable",
	"oom_score_adj":    "OOM settings are not configurable",
	"mac_address":      "MAC addresses are not configurable",
	"external_links":   "only services of this stack resolve by name; reach the external container another way",
	"post_start":       "lifecycle hooks are not supported: fold the command into `command:` or a oneshot service",
	"pre_stop":         "lifecycle hooks are not supported",
	"attach":           "every service's logs are streamed",
	"annotations":      "OCI annotations are not configurable",
	"group_add":        "supplementary groups are not configurable",
	"blkio_config":     "block IO limits are not configurable",
}

func (t *translator) service(cs composeService) *devService {
	s := &devService{name: cs.name, env: map[string]string{}}
	raw := cs.raw
	handled := map[string]bool{}
	get := func(k string) (any, bool) {
		handled[k] = true
		v, ok := raw[k]
		return v, ok
	}

	s.composeWorkdir = str(raw["working_dir"])
	build, hasBuild := get("build")
	imageRaw, hasImage := get("image")
	image := t.expandField(s, "image", str(imageRaw))
	switch {
	case hasBuild:
		s.fromBuild = true
		t.buildService(s, build, get)
		if hasImage {
			s.note(Info, "image", "%q is the tag compose gives the built image; sklp dev runs the source instead and pulls nothing", image)
		}
	case hasImage && image != "" && t.runsRepoInToolchain(raw, image):
		s.fromBuild = true
		t.toolchainService(s, image, raw, get)
	case hasImage && image != "":
		s.image = image
		s.network = "bridge"
		if tag := imageTag(image); tag == "" || tag == "latest" {
			s.note(Info, "image", "%s is not pinned: dev pulls whatever latest is today; pin the version CI and deploy use", image)
		}
	default:
		s.run = placeholderRun(s.name, "neither image: nor build: is set")
		s.note(Manual, "run", "the compose service has neither image: nor build:; write the command that starts it")
	}

	if v, ok := get("entrypoint"); ok {
		t.entrypoint(s, v, raw)
	}
	if v, ok := get("command"); ok && s.image != "" {
		s.command = t.commandList(s, v)
	}
	if v, ok := get("working_dir"); ok {
		if s.image != "" {
			s.note(Manual, "working_dir", "image services cannot change the working directory (%s); the image WORKDIR applies — wrap the command in `sh -c 'cd %s && ...'` if it matters", str(v), str(v))
		} else {
			s.note(Info, "working_dir", "%s is ignored: the service runs in %s (the repo is mounted at /workspace)", str(v), orRoot(s.workdir))
		}
	}
	if v, ok := get("environment"); ok {
		t.environment(s, v)
	}
	if v, ok := get("env_file"); ok {
		t.envFile(s, v)
	}
	ports, _ := get("ports")
	expose, _ := get("expose")
	t.ports(s, ports, expose)
	switch {
	case s.port > 0 && s.servedBy != "":
		s.note(Manual, "port", "compose served the built app with %s on port %d; the dev server of %s listens on its own port (vite 5173, next 3000, ...): set port: to it", s.servedBy, s.port, orRoot(s.workdir))
	case s.port > 0 && s.devServerRun && !devServerPorts[s.port]:
		s.note(Manual, "port", "%d is the port of the built image; `%s` starts the dev server, which often listens elsewhere (vite 5173, next 3000): set port: to the one it prints", s.port, s.run)
	}
	if v, ok := get("volumes"); ok {
		t.volumes(s, v)
	}
	get("depends_on")
	if v, ok := get("healthcheck"); ok {
		t.healthcheck(s, v)
	}
	if v, ok := get("profiles"); ok {
		s.optional = true
		s.note(Manual, "profiles", "compose starts it only with --profile %s; it is imported as `optional: true`, and sklp dev never starts optional services today — remove `optional: true` to run it", strings.Join(strList(v), ","))
	}
	if v, ok := get("networks"); ok {
		t.networks(s, v)
	}
	if v, ok := get("network_mode"); ok {
		s.note(Manual, "network_mode", "%q is not supported: every service joins the stack network and is reached by its name; replace any localhost:<port> that meant another service with <service>:<port>", str(v))
	}
	if v, ok := get("links"); ok {
		t.links(s, v)
	}
	if v, ok := get("extra_hosts"); ok {
		t.extraHosts(s, v)
	}
	if v, ok := get("extends"); ok {
		s.note(Manual, "extends", "extends from another file (%v) is not resolved: copy the inherited fields into this service", v)
	}

	var keys []string
	for k := range raw {
		if !handled[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "x-"):
		case droppedFields[k] != "":
			s.note(Info, k, "dropped: %s", droppedFields[k])
		case manualFields[k] != "":
			s.note(Manual, k, "%s", manualFields[k])
		default:
			s.note(Manual, k, "no sklp dev equivalent for this compose field; check what it does for the service and reproduce it by hand")
		}
	}
	return s
}

func (t *translator) buildService(s *devService, build any, get func(string) (any, bool)) {
	context, dockerfile, target, args := ".", "Dockerfile", "", map[string]any(nil)
	switch b := build.(type) {
	case string:
		context = b
	case map[string]any:
		if c := str(b["context"]); c != "" {
			context = c
		}
		if d := str(b["dockerfile"]); d != "" {
			dockerfile = d
		}
		target = str(b["target"])
		args = asMap(b["args"])
		if _, ok := b["dockerfile_inline"]; ok {
			s.note(Manual, "build", "dockerfile_inline is not inspected; read it for system packages and the start command")
		}
	}
	context = t.expandField(s, "build", context)
	rel, inRepo := repoRelative(context)
	if !inRepo {
		s.run = placeholderRun(s.name, "build context "+context+" is outside the repo")
		s.note(Manual, "build", "build context %s is outside the repo: sklp dev only runs code from the repo; vendor it or run it as an image: service", context)
		return
	}
	s.workdir = strings.TrimPrefix(rel, "./")
	if s.workdir == "." {
		s.workdir = ""
	}
	if target != "" {
		s.note(Info, "build", "build target %q is not used: sklp dev runs the source, not an image stage", target)
	}
	if len(args) > 0 {
		s.note(Manual, "build", "build args %s only existed at image build time; if the app reads them at runtime, add them to env:", strings.Join(sortedKeys(args), ", "))
	}

	dfPath := filepath.Join(t.opts.Root, filepath.FromSlash(s.workdir), filepath.FromSlash(dockerfile))
	if filepath.IsAbs(dockerfile) {
		dfPath = dockerfile
	}
	df, dfFound := readDockerfile(dfPath)

	cmdRaw, hasCmd := get("command")
	runnable, isApp := t.runnableAt(s.workdir)
	if !hasCmd && !isApp && dfFound {
		if refs := df.referencedModules(t.appDirsUnder(s.workdir)); len(refs) == 1 {
			runnable, isApp = t.runnableAt(refs[0])
			if isApp {
				s.workdir = refs[0]
				s.note(Info, "run", "%s builds %s (the only app it references), so the service runs that module", dockerfile, refs[0])
			}
		} else if len(refs) > 1 {
			s.note(Manual, "run", "%s references several apps (%s): keep the one this service is and set run:/workdir: to it", dockerfile, strings.Join(refs, ", "))
		}
	}
	switch {
	case hasCmd:
		s.run = t.commandShell(s, cmdRaw)
		s.note(Info, "run", "taken from compose command:, which ran inside the image; check it works from %s with the toolchain of .sklp/space.yaml", orRoot(s.workdir))
	case isApp:
		s.run = runnable.run
		if runnable.workdir != s.workdir && runnable.workdir != "" {
			s.workdir = runnable.workdir
		}
		s.note(Info, "run", "inferred from %s (%s); the Dockerfile CMD is not used", runnable.manifest, runnable.reason)
	default:
		s.run = placeholderRun(s.name, "no start command found for "+orRoot(s.workdir))
		msg := fmt.Sprintf("no runnable app detected in %s and no compose command:; write the start command", orRoot(s.workdir))
		if dfFound && len(df.copySource) > 0 && isServiceImage(df.finalFrom, df.stages) {
			msg += fmt.Sprintf(". If this Dockerfile only customises %s, build it as an image and use `image:` (sklp dev does not build images; publish it with a pipeline, or use `image: %s` if the copied files (%s) can come from env)", df.finalFrom, df.finalFrom, strings.Join(df.copySource, ", "))
		}
		s.note(Manual, "run", "%s", msg)
	}

	if !dfFound {
		return
	}
	if !hasCmd {
		if isApp {
			s.servedBy = servingImage(df.finalFrom)
		}
		s.devServerRun = isApp && runnable.lang == "node"
	}
	if len(df.aptPkgs) > 0 {
		s.note(Manual, "build", "%s installs apt packages %s: add the ones the app needs at runtime to `runner.packages` in .sklp/space.yaml", dockerfile, strings.Join(df.aptPkgs, " "))
	}
	if len(df.apkPkgs) > 0 {
		s.note(Manual, "build", "%s installs apk (Alpine) packages %s: find their Debian names and add them to `runner.packages` in .sklp/space.yaml", dockerfile, strings.Join(df.apkPkgs, " "))
	}
}

// runsRepoInToolchain spots the compose idiom of running the repo's source
// inside a language image: the repo (or a subdir) is bind-mounted at the
// working directory. Under sklp that is a run: service on the space toolchain.
func (t *translator) runsRepoInToolchain(raw map[string]any, image string) bool {
	if isServiceImage(image, nil) {
		return false
	}
	_, ok := t.repoMountAtWorkdir(raw)
	return ok
}

func (t *translator) repoMountAtWorkdir(raw map[string]any) (string, bool) {
	wd := str(raw["working_dir"])
	if wd == "" {
		return "", false
	}
	for _, item := range anyList(raw["volumes"]) {
		var src, dst string
		switch m := item.(type) {
		case string:
			parts := strings.Split(m, ":")
			if len(parts) < 2 {
				continue
			}
			src, dst = parts[0], parts[1]
		case map[string]any:
			if str(m["type"]) != "bind" {
				continue
			}
			src, dst = str(m["source"]), str(m["target"])
		}
		rel, ok := repoRelative(src)
		if ok && (strings.HasPrefix(src, ".") || src == "") && path.Clean(dst) == path.Clean(wd) {
			return rel, true
		}
	}
	return "", false
}

func (t *translator) toolchainService(s *devService, image string, raw map[string]any, get func(string) (any, bool)) {
	rel, _ := t.repoMountAtWorkdir(raw)
	s.workdir = strings.TrimPrefix(path.Clean(rel), "./")
	if s.workdir == "." {
		s.workdir = ""
	}
	tool, version := toolchainOf(image)
	if tool != "" {
		s.note(Manual, "image", "%s only supplied the toolchain: sklp runs this service on the space toolchain instead — check .sklp/space.yaml pins %s %s (runner.toolchain.%s)", image, tool, orAny(version), tool)
	} else {
		s.note(Manual, "image", "%s ran the repo's source: imported as a run: service on the space toolchain — add what this image provided to .sklp/space.yaml (runner.toolchain / runner.packages)", image)
	}
	cmdRaw, hasCmd := get("command")
	runnable, isApp := t.runnableAt(s.workdir)
	switch {
	case hasCmd:
		s.run = t.commandShell(s, cmdRaw)
		s.note(Info, "run", "taken from compose command:; drop any toolchain setup it does (corepack, apk add, pip install of the tool itself) if the space toolchain already provides it")
	case isApp:
		s.run = runnable.run
		s.note(Info, "run", "the image's default command is unknown here; inferred from %s (%s)", runnable.manifest, runnable.reason)
	default:
		s.run = placeholderRun(s.name, "no start command found for "+orRoot(s.workdir))
		s.note(Manual, "run", "no compose command: and no runnable app detected in %s; write the start command", orRoot(s.workdir))
	}
}

var toolchainNames = map[string]string{"node": "node", "golang": "go", "python": "python", "rust": "rust", "oven/bun": "bun", "denoland/deno": "deno"}

func toolchainOf(image string) (tool, version string) {
	repo, tag := image, imageTag(image)
	if tag != "" {
		repo = strings.TrimSuffix(image, ":"+tag)
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	tool = toolchainNames[repo]
	if tool == "" {
		return "", ""
	}
	version, _, _ = strings.Cut(tag, "-")
	if version == "latest" {
		version = ""
	}
	return tool, version
}

func imageTag(image string) string {
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[at+1:]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

func orAny(v string) string {
	if v == "" {
		return "(the version you use)"
	}
	return v
}

func (t *translator) appDirsUnder(dir string) []string {
	base := path.Clean("./" + dir)
	var out []string
	for _, r := range t.opts.Runnables {
		if !r.IsApp || hiddenPath(r.Module.Path) {
			continue
		}
		p := path.Clean("./" + r.Module.Path)
		if base == "." || strings.HasPrefix(p, base+"/") {
			out = append(out, strings.TrimPrefix(p, "./"))
		}
	}
	return out
}

var devServerPorts = map[int]bool{3000: true, 4200: true, 4321: true, 5173: true, 5174: true, 8080: true}

func servingImage(from string) string {
	repo := strings.TrimPrefix(strings.TrimPrefix(from, "docker.io/"), "library/")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, s := range []string{"nginx", "nginxinc/nginx-unprivileged", "caddy", "httpd", "traefik"} {
		if repo == s {
			return from
		}
	}
	return ""
}

type runnableMatch struct {
	run, workdir, manifest, reason, lang string
}

func (t *translator) runnableAt(dir string) (runnableMatch, bool) {
	for _, r := range t.opts.Runnables {
		if !r.IsApp || path.Clean("./"+r.Module.Path) != path.Clean("./"+dir) {
			continue
		}
		sugg := repofacts.SuggestDevServices([]repofacts.RunnableFacts{r})
		if len(sugg) == 0 || sugg[0].Run == "" {
			continue
		}
		return runnableMatch{run: sugg[0].Run, workdir: sugg[0].Workdir, manifest: path.Join(orDot(r.Module.Path), r.Module.Manifest), reason: r.Reason, lang: r.Lang}, true
	}
	return runnableMatch{}, false
}

func (t *translator) entrypoint(s *devService, v any, raw map[string]any) {
	ep := t.commandList(s, v)
	if s.image != "" {
		s.note(Manual, "entrypoint", "image services cannot replace the image ENTRYPOINT (%s): command: is appended to the image's own ENTRYPOINT. Use an image whose ENTRYPOINT fits, or turn it into a run: service", shellJoin(ep))
		return
	}
	if s.run == "" || strings.HasPrefix(s.run, "echo 'sklp import:") {
		return
	}
	if _, hasCmd := raw["command"]; hasCmd {
		s.run = shellJoin(ep) + " " + s.run
	} else {
		s.run = shellJoin(ep)
	}
	s.note(Info, "entrypoint", "folded into run:")
}

func (t *translator) commandList(s *devService, v any) []string {
	switch c := v.(type) {
	case string:
		words, err := shellSplit(t.expandField(s, "command", c))
		if err != nil {
			s.note(Manual, "command", "could not split %q into arguments (%v): write it as a YAML list", c, err)
			return []string{c}
		}
		return words
	case []any:
		out := make([]string, 0, len(c))
		for _, w := range c {
			out = append(out, t.expandField(s, "command", fmt.Sprint(w)))
		}
		return out
	}
	return nil
}

func (t *translator) commandShell(s *devService, v any) string {
	if c, ok := v.(string); ok {
		return t.expandField(s, "command", c)
	}
	return shellJoin(t.commandList(s, v))
}

func (t *translator) environment(s *devService, v any) {
	env := asMap(v)
	for _, key := range sortedKeys(env) {
		val, isNil := env[key], env[key] == nil
		if isNil {
			t.passThrough(s, key)
			continue
		}
		value := fmt.Sprint(val)
		if !strings.Contains(value, "$") {
			s.env[key] = value
			continue
		}
		if def, hasDef, same := sameNameRef(key, value); same {
			t.sameNameEnv(s, key, def, hasDef)
			continue
		}
		e := t.vars.expand(value)
		t.reportExpansion(s, "environment", key, e)
		s.env[key] = e.value
	}
}

func (t *translator) passThrough(s *devService, key string) {
	switch {
	case t.vars.local[key]:
		t.delegatesToDotenv = true
		s.note(Info, "environment", "%s comes from .env (env_files)", key)
	case t.vars.template[key] != "":
		s.env[key] = t.vars.template[key]
		s.note(Info, "environment", "%s inlined from %s", key, t.vars.templateFrom[key])
	default:
		t.delegatesToDotenv = true
		s.note(Manual, "environment", "compose passed %s through from your shell; sklp dev containers never inherit the shell environment — set %s in .env", key, key)
	}
}

func (t *translator) sameNameEnv(s *devService, key, def string, hasDef bool) {
	switch {
	case t.vars.local[key]:
		t.delegatesToDotenv = true
		s.note(Info, "environment", "%s comes from .env (env_files)", key)
	case t.vars.template[key] != "":
		s.env[key] = t.vars.template[key]
		s.note(Info, "environment", "%s inlined from %s", key, t.vars.templateFrom[key])
	case hasDef:
		e := t.vars.expand(def)
		t.reportExpansion(s, "environment", key, e)
		s.env[key] = e.value
	default:
		t.delegatesToDotenv = true
		s.note(Manual, "environment", "%s has no value in compose, .env or .env.example: set %s in .env", key, key)
	}
}

func (t *translator) reportExpansion(s *devService, field, key string, e expansion) {
	where := field
	if key != "" {
		where = field + " " + key
	}
	if !e.resolved() {
		s.note(Manual, field, "%s references %s, which has no value in compose defaults or .env.example; sklp dev does not interpolate ${VAR}: replace it with the literal value", where, dollarList(e.unresolved))
	}
	if len(e.fromTemplate) > 0 {
		s.note(Info, field, "%s: %s inlined from .env.example", where, strings.Join(e.fromTemplate, ", "))
	}
	if len(e.local) > 0 {
		s.note(Manual, field, "%s: your .env sets %s, which compose used here; dev.yaml holds the compose default instead — edit the value if your .env differs", where, strings.Join(e.local, ", "))
	}
}

func (t *translator) expandField(s *devService, field, value string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	e := t.vars.expand(value)
	t.reportExpansion(s, field, "", e)
	return e.value
}

func (t *translator) envFile(s *devService, v any) {
	var files []string
	for _, item := range anyList(v) {
		p, required := "", true
		switch f := item.(type) {
		case string:
			p = f
		case map[string]any:
			p = str(f["path"])
			if r, ok := f["required"].(bool); ok {
				required = r
			}
		}
		rel, ok := repoRelative(p)
		if !ok {
			s.note(Manual, "env_file", "%s is outside the repo: copy the variables into .env", p)
			continue
		}
		rel = strings.TrimPrefix(rel, "./")
		files = append(files, rel)
		t.stack.envFiles = appendUniqueString(t.stack.envFiles, rel)
		if !required {
			s.note(Info, "env_file", "%s was optional in compose; sklp dev also skips a missing env file", rel)
		}
	}
	if len(files) > 0 {
		s.note(Info, "env_file", "%s moved to the stack-level env_files: its variables now reach every service", strings.Join(files, ", "))
	}
}

type portSpec struct {
	container, published int
	rangeSpec, proto     string
}

var portRE = regexp.MustCompile(`^(?:(\[[^\]]*\]|\d+\.\d+\.\d+\.\d+):)?(?:(\d+(?:-\d+)?)?:)?(\d+(?:-\d+)?)(?:/(\w+))?$`)

func (t *translator) parsePorts(s *devService, v any, field string) []portSpec {
	var out []portSpec
	for _, item := range anyList(v) {
		switch p := item.(type) {
		case map[string]any:
			ps := portSpec{container: atoi(fmt.Sprint(p["target"])), proto: str(p["protocol"])}
			if pub := p["published"]; pub != nil {
				ps.published = atoi(fmt.Sprint(pub))
			}
			if ps.container > 0 {
				out = append(out, ps)
			}
		default:
			spec := t.expandField(s, field, fmt.Sprint(p))
			m := portRE.FindStringSubmatch(spec)
			if m == nil {
				s.note(Manual, field, "could not parse %q; set port: by hand", spec)
				continue
			}
			ps := portSpec{proto: m[4]}
			if strings.Contains(m[3], "-") || strings.Contains(m[2], "-") {
				ps.rangeSpec = spec
			}
			first, _, _ := strings.Cut(m[3], "-")
			ps.container = atoi(first)
			if m[2] != "" {
				pub, _, _ := strings.Cut(m[2], "-")
				ps.published = atoi(pub)
			} else if field == "ports" {
				ps.published = -1
			}
			out = append(out, ps)
		}
	}
	return out
}

func (t *translator) ports(s *devService, portsRaw, exposeRaw any) {
	specs := append(t.parsePorts(s, portsRaw, "ports"), t.parsePorts(s, exposeRaw, "expose")...)
	var extra []int
	for _, ps := range specs {
		if ps.rangeSpec != "" {
			s.note(Manual, "ports", "port range %q: dev.yaml declares one port per service; only %d is imported — reach the others in-stack at %s:<port> or with `sklp dev forward %s %s:<port>`", ps.rangeSpec, ps.container, s.name, t.opts.Stack, s.name)
		}
		if ps.proto != "" && ps.proto != "tcp" {
			s.note(Manual, "ports", "%d/%s: sklp dev routes and forwards TCP only", ps.container, ps.proto)
		}
		if s.port == 0 {
			s.port = ps.container
		} else if ps.container != s.port && !containsInt(extra, ps.container) {
			extra = append(extra, ps.container)
		}
		switch {
		case ps.published > 0 && ps.published != ps.container:
			s.note(Info, "ports", "host port %d (container %d) is not bound by sklp dev: reach it with `sklp dev forward %s %s:%d %d`", ps.published, ps.container, t.opts.Stack, s.name, ps.container, ps.published)
		case ps.published != 0 && t.speaksHTTP(s, ps.container):
			s.note(Info, "ports", "host port %d is not bound by sklp dev: HTTP at http://%s.<scope>.localhost, raw TCP with `sklp dev forward %s %s:%d`", ps.container, s.name, t.opts.Stack, s.name, ps.container)
		case ps.published != 0:
			s.note(Info, "ports", "host port %d is not bound by sklp dev: reach it from the host with `sklp dev forward %s %s:%d`", ps.container, t.opts.Stack, s.name, ps.container)
		}
	}
	if len(extra) > 0 {
		s.note(Info, "ports", "port: is %d (the first one compose lists); %s stay reachable in-stack at %s:<port>, and from the host with `sklp dev forward %s %s:<port>`", s.port, joinInts(extra), s.name, t.opts.Stack, s.name)
	}
}

var tcpOnlyPorts = map[int]bool{5432: true, 3306: true, 6379: true, 27017: true, 4222: true, 5672: true, 9092: true, 11211: true, 1025: true, 6222: true, 26257: true}

func (t *translator) speaksHTTP(s *devService, port int) bool {
	if tcpOnlyPorts[port] {
		return false
	}
	return s.image == "" || imageLogPattern(s.image) == ""
}

func (t *translator) volumes(s *devService, v any) {
	for _, item := range anyList(v) {
		var src, dst, mode, typ string
		switch m := item.(type) {
		case string:
			parts := strings.Split(t.expandField(s, "volumes", m), ":")
			switch len(parts) {
			case 1:
				dst = parts[0]
			case 2:
				src, dst = parts[0], parts[1]
			default:
				src, dst, mode = parts[0], parts[1], strings.Join(parts[2:], ":")
			}
		case map[string]any:
			typ, src, dst = str(m["type"]), t.expandField(s, "volumes", str(m["source"])), str(m["target"])
			if ro, _ := m["read_only"].(bool); ro {
				mode = "ro"
			}
		}
		if typ == "tmpfs" {
			s.note(Manual, "volumes", "tmpfs on %s is not supported: declare a named volume on that path", dst)
			continue
		}
		if s.image == "" {
			t.runVolume(s, src, dst)
			continue
		}
		t.imageVolume(s, src, dst, mode)
	}
}

func (t *translator) imageVolume(s *devService, src, dst, mode string) {
	suffix := ""
	switch {
	case mode == "":
	case hasMode(mode, "ro"):
		suffix = ":ro"
	}
	if extra := strings.Trim(strings.NewReplacer("ro", "", "rw", "", ",", "").Replace(mode), " "); extra != "" {
		s.note(Info, "volumes", "mount options %q on %s dropped (SELinux / Docker Desktop flags)", mode, dst)
	}
	switch {
	case src == "":
		name := s.name + "-" + slug(dst)
		s.volumes = append(s.volumes, name+":"+dst+suffix)
		t.namedVolumes = append(t.namedVolumes, name)
		s.note(Info, "volumes", "anonymous volume on %s imported as named volume %s", dst, name)
	case strings.HasPrefix(src, "~"):
		s.note(Manual, "volumes", "%s", bindAdvice(s.image, src, dst))
	case strings.Contains(src, "docker.sock"):
		s.note(Manual, "volumes", "%s: there is no Docker daemon under sklp; the service cannot drive containers", src)
	case strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.Contains(src, "/"):
		s.note(Manual, "volumes", "%s", bindAdvice(s.image, src, dst))
	default:
		if path.Clean(dst) == "/var/lib/postgresql/data" && postgres18OrUnpinned(s.image) {
			dst = "/var/lib/postgresql"
			s.note(Info, "volumes", "%s moved from /var/lib/postgresql/data to /var/lib/postgresql: %s is PostgreSQL 18 or later (or unpinned), which refuses to start on the old mount point; the volume starts empty, so nothing is lost", src, s.image)
		}
		s.volumes = append(s.volumes, src+":"+dst+suffix)
		t.namedVolumes = append(t.namedVolumes, src)
		t.checkTopVolume(s, src)
	}
}

var pgMajorRE = regexp.MustCompile(`^(\d+)`)

// postgres18OrUnpinned reports whether a postgres image resolves to 18+,
// whose entrypoint refuses data mounted at /var/lib/postgresql/data.
func postgres18OrUnpinned(image string) bool {
	if imageLogPattern(image) != "database system is ready to accept connections" {
		return false
	}
	tag := imageTag(image)
	m := pgMajorRE.FindStringSubmatch(tag)
	if m == nil {
		return !strings.Contains(image, "timescale") && !strings.Contains(image, "postgis") && !strings.Contains(image, "pgvector")
	}
	return atoi(m[1]) >= 18
}

// bindAdvice explains how to replace a host bind, which sklp refuses
// everywhere: a service only mounts named volumes.
func bindAdvice(image, src, dst string) string {
	base := fmt.Sprintf("%s:%s is a host bind mount, and sklp only mounts named volumes. ", src, dst)
	switch {
	case strings.HasPrefix(dst, "/docker-entrypoint-initdb.d"):
		return base + fmt.Sprintf("For init scripts: bake them into an image (`FROM %s` + `COPY %s %s`), or run them from a oneshot run: service that depends on this one (e.g. `psql \"$DATABASE_URL\" -f %s`)", image, strings.TrimPrefix(src, "./"), dst, strings.TrimPrefix(src, "./"))
	case strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~"):
		return base + fmt.Sprintf("The path only exists on this machine: bake what the service needs into an image (`FROM %s` + `COPY`), or drop the mount", image)
	}
	return base + fmt.Sprintf("Bake the file into an image (`FROM %s` + `COPY %s %s`) and use that image, or pass the setting through env", image, strings.TrimPrefix(src, "./"), dst)
}

func (t *translator) checkTopVolume(s *devService, name string) {
	def := asMap(t.proj.volumes[name])
	if ext, _ := def["external"].(bool); ext {
		s.note(Manual, "volumes", "volume %s is external in compose (created outside it); sklp creates it empty — restore its data if the service needs it", name)
	}
	if d := str(def["driver"]); d != "" && d != "local" {
		s.note(Manual, "volumes", "volume %s uses driver %q; sklp volumes are local directories", name, d)
	}
	if opts := asMap(def["driver_opts"]); len(opts) > 0 {
		s.note(Manual, "volumes", "volume %s has driver_opts %v (often a bind or NFS mount in disguise); sklp volumes are plain local directories", name, opts)
	}
}

func (t *translator) runVolume(s *devService, src, dst string) {
	switch {
	case strings.Contains(dst, "node_modules"):
		s.note(Info, "volumes", "%s dropped: sklp dev manages node_modules volumes itself", dst)
	case src == "":
		s.note(Info, "volumes", "anonymous volume on %s dropped: run: services write into the repo mount", dst)
	case strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/"):
		rel, ok := repoRelative(src)
		if !ok {
			s.note(Manual, "volumes", "run: services cannot mount %s:%s; move it into the repo and point the app at its repo path", src, dst)
			return
		}
		if t.mirrorsRepoLayout(s, rel, dst) {
			s.note(Info, "volumes", "%s:%s dropped: the repo is already mounted at /workspace and the service runs from %s", src, dst, orRoot(s.workdir))
			return
		}
		s.note(Manual, "volumes", "%s was mounted at %s; run: services see the repo at /workspace instead, so point the app at /workspace/%s (or the path relative to %s)", src, dst, strings.TrimPrefix(rel, "./"), orRoot(s.workdir))
	default:
		msg := fmt.Sprintf("run: services cannot mount volumes (%s:%s): let the app write under a git-ignored repo path instead", src, dst)
		if vars := envPointingAt(s.env, dst); len(vars) > 0 {
			msg += fmt.Sprintf(" — change %s (now %s) to that path, e.g. .sklp/cache/%s", strings.Join(vars, ", "), dst, s.name)
		}
		s.note(Manual, "volumes", "%s", msg)
	}
}

var (
	healthURLRE   = regexp.MustCompile(`(https?)://(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(?::(\d+))?(/[^\s'"]*)?`)
	knownReadyLog = []struct {
		match []string
		log   string
	}{
		{[]string{"postgres", "postgis", "timescaledb", "timescaledb-ha", "pgvector", "supabase/postgres", "paradedb"}, "database system is ready to accept connections"},
		{[]string{"mysql", "mariadb", "percona"}, "ready for connections"},
		{[]string{"redis", "valkey", "redis-stack", "redis-stack-server", "keydb"}, "Ready to accept connections"},
		{[]string{"mongo"}, "Waiting for connections"},
		{[]string{"nats"}, "Server is ready"},
		{[]string{"rabbitmq"}, "Server startup complete"},
	}
)

func imageLogPattern(image string) string {
	ref := image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if slash := strings.LastIndex(ref, "/"); slash >= 0 {
		if colon := strings.LastIndex(ref, ":"); colon > slash {
			ref = ref[:colon]
		}
	} else if colon := strings.LastIndex(ref, ":"); colon >= 0 {
		ref = ref[:colon]
	}
	base := ref[strings.LastIndex(ref, "/")+1:]
	for _, k := range knownReadyLog {
		for _, m := range k.match {
			if base == m || strings.HasSuffix(ref, "/"+m) || ref == m {
				return k.log
			}
		}
	}
	return ""
}

func (t *translator) healthcheck(s *devService, v any) {
	hc := asMap(v)
	if disabled, _ := hc["disable"].(bool); disabled {
		return
	}
	test := healthTest(hc["test"])
	if test == "" || test == "NONE" {
		return
	}
	timeout := healthTimeout(hc)
	if m := healthURLRE.FindStringSubmatch(test); m != nil {
		p, pathPart := atoi(m[2]), m[3]
		if pathPart == "" {
			pathPart = "/"
		}
		if p == 0 {
			p = 80
		}
		if s.port == 0 {
			s.port = p
		}
		probe := &devReady{timeout: timeout, http: pathPart}
		if p != s.port {
			probe.http = fmt.Sprintf("http://localhost:%d%s", p, pathPart)
		}
		s.healthProbe = probe
		if m[1] == "https" {
			s.note(Manual, "healthcheck", "`%s` probes over HTTPS; sklp dev probes plain HTTP — check the service also answers HTTP on port %d, or set ready.tcp", test, p)
			return
		}
		s.note(Info, "healthcheck", "`%s` imported as an HTTP probe (2xx expected)", test)
		return
	}
	if s.image != "" {
		if logLine := imageLogPattern(s.image); logLine != "" {
			s.healthProbe = &devReady{log: logLine, timeout: timeout}
			s.note(Info, "healthcheck", "`%s` replaced by the log line %q: sklp dev cannot exec into the container", test, logLine)
			return
		}
	}
	if s.port > 0 {
		s.healthProbe = &devReady{tcp: fmt.Sprintf("localhost:%d", s.port), timeout: timeout}
		s.note(Manual, "healthcheck", "`%s` cannot run under sklp dev; replaced by a TCP probe on port %d, which passes as soon as the port opens — if the service is not usable that early, set ready.log to the line it prints once ready", test, s.port)
		return
	}
	s.note(Manual, "healthcheck", "`%s` cannot run under sklp dev and the service has no port: set ready.log to the line the service prints once ready", test)
}

func healthTest(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(strings.TrimPrefix(t, "CMD-SHELL "))
	case []any:
		words := make([]string, 0, len(t))
		for _, w := range t {
			words = append(words, fmt.Sprint(w))
		}
		if len(words) == 0 {
			return ""
		}
		switch words[0] {
		case "NONE":
			return "NONE"
		case "CMD-SHELL":
			return strings.Join(words[1:], " ")
		case "CMD":
			return shellJoin(words[1:])
		}
		return shellJoin(words)
	}
	return ""
}

func healthTimeout(hc map[string]any) string {
	interval := parseDur(str(hc["interval"]), 30)
	retries := 3
	if r, ok := hc["retries"].(int); ok && r > 0 {
		retries = r
	}
	total := parseDur(str(hc["start_period"]), 0) + interval*retries
	if total < 30 {
		total = 30
	}
	if total > 300 {
		total = 300
	}
	return strconv.Itoa(total) + "s"
}

func (t *translator) networks(s *devService, v any) {
	switch n := v.(type) {
	case []any:
		if len(n) > 1 {
			s.note(Info, "networks", "networks %s merged: sklp dev has one network per stack, where every service reaches every other", strings.Join(strList(n), ", "))
		}
	case map[string]any:
		if len(n) > 1 {
			s.note(Info, "networks", "networks %s merged: sklp dev has one network per stack, where every service reaches every other", strings.Join(sortedKeys(n), ", "))
		}
		for _, name := range sortedKeys(n) {
			cfg := asMap(n[name])
			if aliases := strList(cfg["aliases"]); len(aliases) > 0 {
				s.note(Manual, "networks", "aliases %s do not resolve in sklp dev, only %q does: replace those hostnames in the other services' config", strings.Join(aliases, ", "), s.name)
			}
			for _, k := range []string{"ipv4_address", "ipv6_address"} {
				if ip := str(cfg[k]); ip != "" {
					s.note(Manual, "networks", "fixed address %s is not supported: reach the service by its name %q", ip, s.name)
				}
			}
		}
	}
}

func (t *translator) links(s *devService, v any) {
	for _, l := range strList(v) {
		target, alias, hasAlias := strings.Cut(l, ":")
		if hasAlias && alias != target {
			s.note(Manual, "links", "alias %q for %s does not resolve: use the hostname %q instead", alias, target, target)
		}
	}
	s.note(Info, "links", "dropped: every service already resolves every other by name")
}

func (t *translator) extraHosts(s *devService, v any) {
	var hosts []string
	switch h := v.(type) {
	case []any:
		hosts = strList(h)
	case map[string]any:
		for _, k := range sortedKeys(h) {
			hosts = append(hosts, k+":"+fmt.Sprint(h[k]))
		}
	}
	for _, h := range hosts {
		name, target, _ := strings.Cut(strings.Replace(h, "=", ":", 1), ":")
		if target == "host-gateway" {
			t.stack.allowHost = true
			s.note(Manual, "extra_hosts", "%s → host-gateway: `allowHost: true` is set so the service can reach your machine, but %s does not resolve — use your machine's LAN IP in its config", name, name)
			continue
		}
		s.note(Manual, "extra_hosts", "%s → %s cannot be added to /etc/hosts: use the address directly in the service config", name, target)
	}
}

func (t *translator) dependsOn(cs composeService) {
	s := t.stack.find(cs.name)
	deps := asMap(cs.raw["depends_on"])
	for _, dep := range sortedKeys(deps) {
		target := t.stack.find(dep)
		if target == nil {
			s.note(Manual, "depends_on", "%s is not a service of this compose project: remove the dependency or add the service", dep)
			continue
		}
		cfg := asMap(deps[dep])
		switch cond := str(cfg["condition"]); cond {
		case "service_completed_successfully":
			if !target.oneshot {
				target.oneshot = true
				target.note(Info, "oneshot", "marked oneshot: %s waits for it to exit 0 (condition service_completed_successfully)", s.name)
			}
		case "service_healthy":
			target.needsHealthy = true
		}
		if req, ok := cfg["required"].(bool); ok && !req {
			s.note(Info, "depends_on", "%s was optional (required: false); sklp dev always waits for it", dep)
		}
		if target.optional && !s.optional {
			s.note(Manual, "depends_on", "%s depends on %s, which is optional and never started: remove `optional: true` from %s or drop the dependency", s.name, dep, dep)
		}
		s.dependsOn = append(s.dependsOn, dep)
	}
}

func (t *translator) readiness() {
	dependents := map[string]bool{}
	for _, s := range t.stack.services {
		for _, d := range s.dependsOn {
			dependents[d] = true
		}
	}
	for _, s := range t.stack.services {
		if s.oneshot {
			if s.healthProbe != nil {
				s.note(Info, "healthcheck", "dropped: a oneshot signals readiness by exiting 0")
			}
			continue
		}
		if !dependents[s.name] && s.healthProbe == nil {
			continue
		}
		switch {
		case s.healthProbe != nil:
			s.ready = s.healthProbe
		case s.image != "" && imageLogPattern(s.image) != "":
			s.ready = &devReady{log: imageLogPattern(s.image), timeout: "60s"}
		case s.port > 0:
			s.ready = &devReady{tcp: fmt.Sprintf("localhost:%d", s.port), timeout: "60s"}
			if s.needsHealthy {
				s.note(Manual, "ready", "dependents waited for it to be healthy, but it has no healthcheck sklp can run; the TCP probe on port %d passes as soon as the port opens", s.port)
			}
		default:
			s.note(Manual, "ready", "other services depend on it but it has no port nor known log line: set ready.log to the line it prints once ready, or dependents wait on sklp's generic log pattern and may time out")
		}
	}
}

func (t *translator) uncoveredRunnables() {
	covered := map[string]bool{}
	for _, s := range t.stack.services {
		if s.fromBuild {
			covered[path.Clean("./"+s.workdir)] = true
		}
	}
	var infra []string
	for _, s := range t.stack.services {
		if s.image != "" && !s.optional && !s.oneshot {
			infra = append(infra, s.name)
		}
	}
	composeRunsApps := len(covered) > 0
	var skipped []string
	for _, r := range t.opts.Runnables {
		if !r.IsApp || covered[path.Clean("./"+r.Module.Path)] || hiddenPath(r.Module.Path) {
			continue
		}
		if composeRunsApps {
			skipped = append(skipped, orDot(r.Module.Path))
			continue
		}
		for _, sugg := range repofacts.SuggestDevServices([]repofacts.RunnableFacts{r}) {
			name := sugg.Name
			for i := 2; t.stack.find(name) != nil; i++ {
				name = fmt.Sprintf("%s-%d", sugg.Name, i)
			}
			s := &devService{name: name, run: sugg.Run, workdir: sugg.Workdir, env: map[string]string{}, detected: true}
			s.note(Info, "", "not in compose: detected from %s (%s)", path.Join(orDot(r.Module.Path), r.Module.Manifest), r.Reason)
			msg := "set port: to the port it listens on, so other services and the host browser can reach it"
			if len(infra) > 0 {
				msg += fmt.Sprintf("; add depends_on for what it needs at boot (compose services: %s)", strings.Join(infra, ", "))
			}
			s.note(Manual, "", "%s", msg)
			t.stack.services = append(t.stack.services, s)
		}
	}
	if len(skipped) > 0 {
		t.stackNote(Info, "services", "apps detected in the repo but absent from compose, left out: %s — add them if they belong to the dev stack", strings.Join(skipped, ", "))
	}
}

func (t *translator) topLevel() {
	keys := sortedKeys(t.proj.topLevel)
	for _, k := range keys {
		switch {
		case k == "version" || k == "name" || k == "networks" || strings.HasPrefix(k, "x-"):
		case k == "include":
			t.stackNote(Manual, "include", "included compose files %v are not imported: re-run on a merged file (`docker compose config > compose.yaml`) or add their services by hand", t.proj.topLevel[k])
		case k == "secrets" || k == "configs":
			t.stackNote(Manual, k, "top-level %s are not supported: pass values through .env, bake files into the image (`COPY`)", k)
		default:
			t.stackNote(Manual, k, "top-level compose key with no sklp equivalent")
		}
	}
}

// mirrorsRepoLayout reports whether a compose bind reproduces what sklp
// already does for a run: service, the build context mounted at the
// container's working directory.
func (t *translator) mirrorsRepoLayout(s *devService, rel, dst string) bool {
	ctx := path.Clean("./" + s.workdir)
	src := path.Clean(rel)
	if src != ctx && !strings.HasPrefix(src, ctx+"/") && ctx != "." {
		return false
	}
	if s.composeWorkdir == "" {
		return src == ctx
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(src, ctx), "/")
	if ctx == "." {
		sub = strings.TrimPrefix(src, "./")
		if src == "." {
			sub = ""
		}
	}
	return path.Clean(dst) == path.Clean(path.Join(s.composeWorkdir, sub))
}

func envPointingAt(env map[string]string, dir string) []string {
	var out []string
	for k, v := range env {
		if v == dir || strings.HasPrefix(v, strings.TrimSuffix(dir, "/")+"/") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func hiddenPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." && seg != ".." {
			return true
		}
	}
	return false
}

func placeholderRun(name, why string) string {
	return fmt.Sprintf("echo 'sklp import: %s has no start command yet (%s); see TODO(import) in this file' >&2; exit 1", name, strings.ReplaceAll(why, "'", ""))
}

func repoRelative(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || strings.Contains(p, "://") {
		return "", false
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if clean == "." {
		return ".", true
	}
	return "./" + clean, true
}

var toolchainImages = []string{"golang", "node", "python", "rust", "ruby", "php", "openjdk", "eclipse-temurin", "maven", "gradle", "denoland/deno", "oven/bun", "mcr.microsoft.com/dotnet", "elixir", "alpine", "debian", "ubuntu", "busybox", "scratch", "gcr.io/distroless"}

// isServiceImage reports whether a Dockerfile's final base is a ready-made
// service (nginx, postgres) rather than a language toolchain or a bare OS,
// i.e. whether the build only customises an off-the-shelf image.
func isServiceImage(from string, stages []string) bool {
	if from == "" || strings.Contains(from, "$") {
		return false
	}
	for _, st := range stages {
		if strings.EqualFold(st, from) {
			return false
		}
	}
	repo := from
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, tc := range toolchainImages {
		if repo == tc || strings.HasPrefix(repo, tc+"/") {
			return false
		}
	}
	return true
}

func hasMode(mode, want string) bool {
	for _, m := range strings.Split(mode, ",") {
		if strings.TrimSpace(m) == want {
			return true
		}
	}
	return false
}

var slugRE = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slug(p string) string {
	s := strings.Trim(slugRE.ReplaceAllString(p, "-"), "-")
	if s == "" {
		return "data"
	}
	return strings.ToLower(s)
}

func parseDur(s string, def int) int {
	if s == "" {
		return def
	}
	total, num := 0, 0
	unit := map[byte]int{'s': 1, 'm': 60, 'h': 3600}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
		case c == 'm' && i+1 < len(s) && s[i+1] == 's':
			num = 0
			i++
		case unit[c] > 0:
			total += num * unit[c]
			num = 0
		default:
			return def
		}
	}
	return total + num
}

func str(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return fmt.Sprint(v)
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func anyList(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case nil:
		return nil
	}
	return []any{v}
}

func strList(v any) []string {
	var out []string
	for _, item := range anyList(v) {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func joinInts(list []int) string {
	parts := make([]string, len(list))
	for i, n := range list {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

func dollarList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		if strings.HasPrefix(n, "$") {
			out[i] = n
		} else {
			out[i] = "${" + n + "}"
		}
	}
	return strings.Join(out, ", ")
}

func appendUniqueString(list []string, items ...string) []string {
	for _, it := range items {
		dup := false
		for _, x := range list {
			if x == it {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, it)
		}
	}
	return list
}

func orRoot(dir string) string {
	if dir == "" || dir == "." {
		return "the repo root"
	}
	return dir
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}
