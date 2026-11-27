package importer

import (
	"bytes"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var composeProdOverrideNames = []string{
	"compose.prod.yaml", "compose.prod.yml", "compose.production.yaml", "compose.production.yml",
	"docker-compose.prod.yaml", "docker-compose.prod.yml", "docker-compose.production.yaml", "docker-compose.production.yml",
}

// FindDeployComposeFiles returns the base compose file and its production
// override. The default override is left out: by convention it carries the
// developer's local tweaks, not what runs in production.
func FindDeployComposeFiles(root string) (files []string, skippedOverride string) {
	dev := FindComposeFiles(root)
	if len(dev) == 0 {
		return nil, ""
	}
	files = []string{dev[0]}
	for _, o := range composeProdOverrideNames {
		if isFile(filepath.Join(root, o)) {
			files = append(files, o)
			break
		}
	}
	if len(dev) > 1 {
		skippedOverride = dev[1]
	}
	return files, skippedOverride
}

// ImportComposeDeploy translates the compose files into a production
// `.sklp/<stack>/deploy.yaml`. It returns nil when root holds no compose file.
func ImportComposeDeploy(opts ComposeOptions) (*ComposeImport, error) {
	files, skipped := FindDeployComposeFiles(opts.Root)
	if len(files) == 0 {
		return nil, nil
	}
	proj, err := loadComposeProject(opts.Root, files)
	if err != nil {
		return nil, err
	}
	proj.resolveExtends()
	if opts.Stack == "" {
		opts.Stack = "<stack>"
	}
	project := slug(filepath.Base(absOr(opts.Root)))
	d := &deployTranslator{
		translator: &translator{opts: opts, proj: proj, vars: loadVarSource(opts.Root), stack: &devStack{}},
		st:         &deployStack{origin: "compose", name: opts.Stack, space: project + "-production", project: project, vars: map[string]*deployVar{}},
	}
	if skipped != "" {
		d.stackNote(Info, "files", "%s is not imported: the default override holds local development tweaks; put production-only settings in compose.prod.yaml and import again", skipped)
	}
	d.translate()
	return &ComposeImport{Sources: files, YAML: renderDeploy(d.st, files), Notes: d.st.allNotes()}, nil
}

func absOr(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

type deployStack struct {
	name, space, project string
	origin               string
	registries           []deployRegistry
	envFiles             []string
	services             []*deployService
	vars                 map[string]*deployVar
	notes                []Note
}

func (st *deployStack) find(name string) *deployService {
	for _, s := range st.services {
		if s.name == name {
			return s
		}
	}
	return nil
}

func (st *deployStack) allNotes() []Note {
	out := append([]Note(nil), st.notes...)
	for _, s := range st.services {
		out = append(out, s.notes...)
	}
	return out
}

func (st *deployStack) hasRoutes() bool {
	for _, s := range st.services {
		if len(s.routes) > 0 {
			return true
		}
	}
	return false
}

type deployRegistry struct {
	host, username, password string
}

type deployVar struct {
	name, def, file string
	secret, hasDef  bool
	local           bool
}

type deployRoute struct {
	host, path      string
	compress        bool
	securityHeaders bool
}

type deployService struct {
	name         string
	image        string
	command      []string
	replicas     int
	port         int
	ports        []string
	env          map[string]string
	volumes      []string
	dependsOn    []string
	capabilities []string
	cpu, memory  string
	ready        *devReady
	backup       []string
	job          bool
	schedule     string
	scratch      []string
	waitsFor     []string
	routes       []deployRoute
	routePort    int
	fromBuild    bool
	healthProbe  *devReady
	needsHealthy bool
	notes        []Note
}

func (s *deployService) note(sev Severity, field, format string, args ...any) {
	s.notes = append(s.notes, Note{Severity: sev, Service: s.name, Field: field, Message: fmt.Sprintf(format, args...)})
}

type deployTranslator struct {
	*translator
	st *deployStack
}

func (d *deployTranslator) stackNote(sev Severity, field, format string, args ...any) {
	d.st.stackNote(sev, field, format, args...)
}

func (st *deployStack) stackNote(sev Severity, field, format string, args ...any) {
	st.notes = append(st.notes, Note{Severity: sev, Field: field, Message: fmt.Sprintf(format, args...)})
}

var deployDroppedFields = map[string]string{
	"container_name":    "the service name is its hostname inside the space",
	"restart":           "the node restarts a service that exits, with backoff; a run-to-completion task belongs under jobs:",
	"logging":           "the node ships every service's stdout/stderr itself",
	"stdin_open":        "no stdin is attached to a deployed service",
	"tty":               "no TTY allocation",
	"init":              "sklp runs its own init as PID 1",
	"stop_signal":       "the node always stops with SIGTERM",
	"stop_grace_period": "the node drains then sends SIGTERM; tune ready.drain if the service needs time to finish requests",
	"pull_policy":       "the node pulls the exact tag the file names",
	"hostname":          "the service name is the hostname",
	"domainname":        "the service name is the hostname",
	"read_only":         "the rootfs is already read-only; declare writable paths as volumes",
	"develop":           "compose watch is a development feature",
	"platform":          "the node pulls the image for its own architecture: publish a matching one",
	"mem_reservation":   "reservations are not modelled: resources: sets limits",
	"cpu_shares":        "relative CPU shares are not modelled: resources.cpu sets a limit",
	"cpuset":            "CPU pinning is not configurable",
}

var deployManualFields = map[string]string{
	"secrets":        "compose secrets are files under /run/secrets; a deployed service gets them through env: write `KEY: ${KEY}` and store KEY as a secret",
	"configs":        "compose configs are not supported: bake the file into the image (`COPY`) or pass the setting through env",
	"external_links": "only services of this deploy resolve by name; reach the external container through its public address",
	"working_dir":    "deploy.yaml has no working_dir: the image WORKDIR applies — set it in the Dockerfile or wrap the command in `sh -c 'cd <dir> && ...'`",
}

var edgeProxyImages = []string{"traefik", "caddy", "nginxproxy/nginx-proxy", "jwilder/nginx-proxy", "jc21/nginx-proxy-manager", "nginxproxy/acme-companion", "lucaslorentz/caddy-docker-proxy", "containrrr/watchtower"}

func edgeProxy(image string) bool {
	repo := image
	if tag := imageTag(image); tag != "" {
		repo = strings.TrimSuffix(strings.TrimSuffix(image, ":"+tag), "@"+tag)
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, p := range edgeProxyImages {
		if repo == p {
			return true
		}
	}
	return false
}

func (d *deployTranslator) translate() {
	var profiled []string
	for _, cs := range d.proj.services {
		if _, ok := cs.raw["profiles"]; ok {
			profiled = append(profiled, cs.name)
			continue
		}
		if s := d.service(cs); s != nil {
			d.st.services = append(d.st.services, s)
		}
	}
	if len(profiled) > 0 {
		d.stackNote(Info, "profiles", "services behind a compose profile are left out of production: %s — add them by hand if they belong there", strings.Join(profiled, ", "))
	}
	for _, cs := range d.proj.services {
		if s := d.st.find(cs.name); s != nil {
			d.dependsOn(s, cs)
		}
	}
	d.st.readiness()
	d.st.backups()
	d.topLevel()
	d.st.finish()
}

func (st *deployStack) finish() {
	if st.hasRoutes() {
		st.ref("ACME_EMAIL", "", false)
		st.stackNote(Info, "gateway", "the node gateway terminates TLS for every route with a Let's Encrypt certificate requested for ACME_EMAIL; point each route host's DNS at the node")
		st.stackNote(Info, "spaces", "routes live in space %s: rename it to the space this deploy should own", st.space)
	}
	st.varsNote()
}

func (d *deployTranslator) service(cs composeService) *deployService {
	s := &deployService{name: cs.name, env: map[string]string{}}
	raw := cs.raw
	handled := map[string]bool{}
	get := func(k string) (any, bool) {
		handled[k] = true
		v, ok := raw[k]
		return v, ok
	}

	build, hasBuild := get("build")
	imageRaw, hasImage := get("image")
	image := d.convert(s, "image", "", str(imageRaw))
	switch {
	case hasBuild:
		s.fromBuild = true
		d.buildImage(s, build, image, hasImage)
	case hasImage && image != "":
		if edgeProxy(image) {
			d.stackNote(Manual, "services", "%s (%s) is the edge proxy of the compose setup: it is left out, the node gateway terminates TLS and routes hosts — check the routes under spaces: cover what it served", s.name, image)
			return nil
		}
		s.image = image
		if tag := imageTag(image); tag == "" || tag == "latest" {
			s.note(Manual, "image", "%s is not pinned: production must name an exact version (a rollout only changes what the file says)", image)
		}
	default:
		s.image = placeholderImage(d.st.project, s.name)
		s.note(Manual, "image", "the compose service has neither image: nor build:; set the image it runs")
	}

	if v, ok := get("entrypoint"); ok {
		s.note(Manual, "entrypoint", "deploy.yaml cannot replace the image ENTRYPOINT (%s): command: is appended to it — bake the entrypoint into the image", shellJoin(d.args(s, "entrypoint", v)))
	}
	if v, ok := get("command"); ok {
		s.command = d.args(s, "command", v)
		if s.fromBuild && devCommandRE.MatchString(strings.Join(s.command, " ")) {
			s.note(Manual, "command", "`%s` starts a development server: production runs the built image's own start command — drop command: or set the production one", shellJoin(s.command))
		}
	}
	if v, ok := raw["working_dir"]; ok && s.fromBuild {
		handled["working_dir"] = true
		s.note(Info, "working_dir", "%s dropped: the published image's WORKDIR applies", str(v))
	}
	if v, ok := get("environment"); ok {
		d.environment(s, v)
	}
	if v, ok := get("env_file"); ok {
		d.envFile(s, v)
	}
	ports, _ := get("ports")
	expose, _ := get("expose")
	d.ports(s, ports, expose)
	if v, ok := get("labels"); ok {
		d.labels(s, v)
	}
	if len(s.routes) > 0 {
		if s.routePort > 0 {
			s.port = s.routePort
		}
		if s.port == 0 {
			s.note(Manual, "port", "the service is routed but declares no port: set port: to the one it listens on")
		}
	}
	if v, ok := get("volumes"); ok {
		d.volumes(s, v)
	}
	get("depends_on")
	if v, ok := get("healthcheck"); ok {
		d.healthcheck(s, v)
	}
	d.limits(s, get)
	if v, ok := get("cap_add"); ok {
		s.capabilities = strList(v)
		s.note(Info, "cap_add", "copied to capabilities:; the node's admission policy refuses the dangerous ones (SYS_ADMIN, NET_ADMIN, ...) at apply")
	}
	if v, ok := get("networks"); ok {
		d.networks(s, v)
	}
	if v, ok := get("network_mode"); ok {
		s.note(Manual, "network_mode", "%q is not supported: every service joins the space network and is reached by its name", str(v))
	}
	if _, ok := get("links"); ok {
		s.note(Info, "links", "dropped: every service already resolves every other by name")
	}
	if v, ok := get("extra_hosts"); ok {
		s.note(Manual, "extra_hosts", "%v cannot be added to /etc/hosts: use the addresses directly in the service config", v)
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
		case deployDroppedFields[k] != "":
			s.note(Info, k, "dropped: %s", deployDroppedFields[k])
		case deployManualFields[k] != "":
			s.note(Manual, k, "%s", deployManualFields[k])
		case manualFields[k] != "":
			s.note(Manual, k, "%s", manualFields[k])
		default:
			s.note(Manual, k, "no deploy.yaml equivalent for this compose field; check what it does for the service and reproduce it by hand")
		}
	}
	return s
}

func placeholderImage(project, service string) string {
	return "registry.example.com/" + project + "/" + service + ":unpublished"
}

var devCommandRE = regexp.MustCompile(`(?i)(\bgo run\b|\b(npm|pnpm|yarn|bun)( run)? dev\b|\bnodemon\b|\bair\b|\bvite\b|\bnext dev\b|--reload\b|\bflask run\b|\brunserver\b|\bwatch\b)`)

func (d *deployTranslator) buildImage(s *deployService, build any, image string, hasImage bool) {
	context := "."
	switch b := build.(type) {
	case string:
		context = b
	case map[string]any:
		if c := str(b["context"]); c != "" {
			context = c
		}
		if args := asMap(b["args"]); len(args) > 0 {
			s.note(Info, "build", "build args %s are a publish concern now: pass them as build_args in the publish pipeline", strings.Join(sortedKeys(args), ", "))
		}
	}
	if hasImage && image != "" {
		s.image = image
		s.note(Manual, "image", "compose builds %s from %s: publish it to your registry with an immutable tag (a `kind: publish` pipeline under .sklp/pipelines/) and set image: to that tag", image, context)
		return
	}
	s.image = placeholderImage(d.st.project, s.name)
	s.note(Manual, "image", "compose builds this service from %s: publish the image to your registry with an immutable tag (a `kind: publish` pipeline under .sklp/pipelines/), declare the registry under registries:, and set image: to it", context)
}

func (d *deployTranslator) args(s *deployService, field string, v any) []string {
	switch c := v.(type) {
	case string:
		words, err := shellSplit(c)
		if err != nil {
			s.note(Manual, field, "could not split %q into arguments (%v): write it as a YAML list", c, err)
			return []string{d.convert(s, field, "", c)}
		}
		for i, w := range words {
			words[i] = d.convert(s, field, "", w)
		}
		return words
	case []any:
		out := make([]string, 0, len(c))
		for _, w := range c {
			out = append(out, d.convert(s, field, "", fmt.Sprint(w)))
		}
		return out
	}
	return nil
}

func (d *deployTranslator) environment(s *deployService, v any) {
	env := asMap(v)
	for _, key := range sortedKeys(env) {
		if env[key] == nil {
			s.env[key] = d.ref(key, "", false)
			continue
		}
		value := fmt.Sprint(env[key])
		if secretNameRE.MatchString(key) && !strings.Contains(value, "$") {
			s.env[key] = d.ref(key, value, true)
			s.note(Info, "environment", "%s was written in clear in compose: it is now ${%s}, a secret to set per deployment", key, key)
			continue
		}
		s.env[key] = d.convert(s, "environment", key, value)
	}
}

func (d *deployTranslator) envFile(s *deployService, v any) {
	for _, item := range anyList(v) {
		p := ""
		switch f := item.(type) {
		case string:
			p = f
		case map[string]any:
			p = str(f["path"])
		}
		rel, ok := repoRelative(p)
		if !ok {
			s.note(Manual, "env_file", "%s is outside the repo: list the variables the service needs under env: as `KEY: ${KEY}`", p)
			continue
		}
		rel = strings.TrimPrefix(rel, "./")
		keys := readDotenv(filepath.Join(d.opts.Root, filepath.FromSlash(rel)))
		if len(keys) == 0 {
			s.note(Manual, "env_file", "%s is missing or empty here: list the variables the service needs under env: as `KEY: ${KEY}`", rel)
			continue
		}
		var names []string
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if _, set := s.env[k]; set {
				continue
			}
			s.env[k] = d.ref(k, "", false)
			d.st.vars[k].file = rel
		}
		d.st.envFiles = appendUniqueString(d.st.envFiles, rel)
		s.note(Info, "env_file", "%s: each of its keys is passed as `KEY: ${KEY}`, and the file is listed under env_files: so sklp deploy reads the values from it on the machine that runs it", rel)
	}
}

func (d *deployTranslator) ports(s *deployService, portsRaw, exposeRaw any) {
	specs := append(d.parsePorts(s, portsRaw, "ports"), d.parsePorts(s, exposeRaw, "expose")...)
	datastore := s.image != "" && imageLogPattern(s.image) != ""
	for _, ps := range specs {
		if s.port == 0 && (ps.proto == "" || ps.proto == "tcp") {
			s.port = ps.container
		}
		if ps.published == 0 {
			continue
		}
		switch {
		case ps.rangeSpec != "":
			s.ports = append(s.ports, "0.0.0.0:"+strings.TrimPrefix(ps.rangeSpec, "0.0.0.0:"))
			s.note(Info, "ports", "range %s published on every host address, as compose did", ps.rangeSpec)
		case ps.proto == "udp":
			s.ports = append(s.ports, fmt.Sprintf("0.0.0.0:%d:%d/udp", hostPort(ps), ps.container))
			s.note(Info, "ports", "UDP %d (container %d) published on every host address, as compose did", hostPort(ps), ps.container)
		case datastore || tcpOnlyPorts[ps.container]:
			s.note(Info, "ports", "host port %d is not published: the other services reach it at %s:%d inside the space, and a datastore stays off the public interface", hostPort(ps), s.name, ps.container)
		case s.fromBuild && viteOnlyPorts[ps.container]:
			s.addRoute(ps.container)
			s.note(Manual, "port", "%d is a development server port: the production image serves on its own port — set port: to it", ps.container)
		default:
			s.addRoute(ps.container)
		}
	}
	if len(s.routes) > 0 {
		s.note(Manual, "routes", "published HTTP port %d becomes a gateway route on %s: set the public hostname under spaces: (or add a traefik/caddy Host label and import again)", s.routePort, s.routes[0].host)
	}
}

var viteOnlyPorts = map[int]bool{4200: true, 4321: true, 5173: true, 5174: true}

func hostPort(ps portSpec) int {
	if ps.published > 0 {
		return ps.published
	}
	return ps.container
}

func (s *deployService) addRoute(port int) {
	if len(s.routes) > 0 {
		return
	}
	s.routePort = port
	s.routes = []deployRoute{{host: s.name + ".example.com"}}
}

var (
	traefikRuleRE = regexp.MustCompile(`^traefik\.http\.routers\.[^.]+\.rule$`)
	traefikPortRE = regexp.MustCompile(`^traefik\.http\.services\.[^.]+\.loadbalancer\.server\.port$`)
	hostMatchRE   = regexp.MustCompile("Host\\(([^)]*)\\)")
	pathMatchRE   = regexp.MustCompile("PathPrefix\\(\\s*[`\"']([^`\"']+)[`\"']\\s*\\)")
	quotedRE      = regexp.MustCompile("[`\"']([^`\"']+)[`\"']")
	caddyLabelRE  = regexp.MustCompile(`^caddy(_\d+)?$`)
	caddyUpRE     = regexp.MustCompile(`^caddy(_\d+)?\.reverse_proxy$`)
	upstreamPort  = regexp.MustCompile(`:(\d+)\b`)
)

func (d *deployTranslator) labels(s *deployService, v any) {
	labels := map[string]string{}
	switch l := v.(type) {
	case map[string]any:
		for k, val := range l {
			labels[k] = d.convert(s, "labels", k, str(val))
		}
	case []any:
		for _, item := range l {
			k, val, _ := strings.Cut(fmt.Sprint(item), "=")
			labels[k] = d.convert(s, "labels", k, val)
		}
	}
	var routes []deployRoute
	port := 0
	for _, k := range sortedStringKeys(labels) {
		val := labels[k]
		switch {
		case traefikRuleRE.MatchString(k):
			p := ""
			if m := pathMatchRE.FindStringSubmatch(val); m != nil {
				p = m[1]
			}
			for _, hm := range hostMatchRE.FindAllStringSubmatch(val, -1) {
				for _, q := range quotedRE.FindAllStringSubmatch(hm[1], -1) {
					routes = append(routes, deployRoute{host: q[1], path: p})
				}
			}
		case traefikPortRE.MatchString(k):
			port = atoi(val)
		case caddyLabelRE.MatchString(k):
			for _, h := range strings.FieldsFunc(val, func(r rune) bool { return r == ' ' || r == ',' }) {
				h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
				if h != "" && !strings.Contains(h, "{") {
					routes = append(routes, deployRoute{host: h})
				}
			}
		case caddyUpRE.MatchString(k):
			if m := upstreamPort.FindStringSubmatch(val); m != nil {
				port = atoi(m[1])
			}
		}
	}
	if len(routes) == 0 {
		s.note(Info, "labels", "dropped: labels carry no meaning on a node")
		return
	}
	s.notes = dropNotes(s.notes, "routes")
	s.routes = routes
	if port > 0 {
		s.routePort = port
	} else if s.routePort == 0 {
		s.routePort = s.port
	}
	hosts := make([]string, len(routes))
	for i, r := range routes {
		hosts[i] = r.host + r.path
	}
	s.note(Info, "labels", "routes %s taken from the reverse-proxy labels; the other labels are dropped", strings.Join(hosts, ", "))
}

func dropNotes(notes []Note, field string) []Note {
	out := notes[:0]
	for _, n := range notes {
		if n.Field != field {
			out = append(out, n)
		}
	}
	return out
}

func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (d *deployTranslator) volumes(s *deployService, v any) {
	for _, item := range anyList(v) {
		var src, dst, mode, typ string
		switch m := item.(type) {
		case string:
			parts := strings.Split(d.expandField(s, "volumes", m), ":")
			switch len(parts) {
			case 1:
				dst = parts[0]
			case 2:
				src, dst = parts[0], parts[1]
			default:
				src, dst, mode = parts[0], parts[1], strings.Join(parts[2:], ":")
			}
		case map[string]any:
			typ, src, dst = str(m["type"]), d.expandField(s, "volumes", str(m["source"])), str(m["target"])
			if ro, _ := m["read_only"].(bool); ro {
				mode = "ro"
			}
		}
		suffix := ""
		if hasMode(mode, "ro") {
			suffix = ":ro"
		}
		bind := strings.HasPrefix(src, "/") || strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~") || strings.Contains(src, "/")
		switch {
		case typ == "tmpfs":
			s.note(Manual, "volumes", "tmpfs on %s is not supported: declare a named volume on that path", dst)
		case s.fromBuild && (src == "" || bind):
			s.note(Info, "volumes", "%s dropped: in production the code and its dependencies come from the published image", strings.TrimPrefix(src+":"+dst, ":"))
		case strings.Contains(src, "docker.sock"):
			s.note(Manual, "volumes", "%s: there is no Docker daemon on a node; the service cannot drive containers", src)
		case bind:
			s.note(Manual, "volumes", "%s", bindAdvice(s.image, src, dst))
		case src == "":
			name := s.name + "-" + slug(dst)
			s.volumes = append(s.volumes, name+":"+dst+suffix)
			s.note(Info, "volumes", "anonymous volume on %s imported as named volume %s", dst, name)
		default:
			if path.Clean(dst) == "/var/lib/postgresql/data" && postgres18OrUnpinned(s.image) {
				dst = "/var/lib/postgresql"
				s.note(Info, "volumes", "%s moved from /var/lib/postgresql/data to /var/lib/postgresql: %s is PostgreSQL 18 or later (or unpinned), which refuses to start on the old mount point", src, s.image)
			}
			s.volumes = append(s.volumes, src+":"+dst+suffix)
			d.checkVolume(s, src)
		}
	}
}

func (d *deployTranslator) checkVolume(s *deployService, name string) {
	def := asMap(d.proj.volumes[name])
	if ext, _ := def["external"].(bool); ext {
		s.note(Manual, "volumes", "volume %s is external in compose: the node creates it empty — restore its data before the first start", name)
	}
	if drv := str(def["driver"]); drv != "" && drv != "local" {
		s.note(Manual, "volumes", "volume %s uses driver %q; node volumes are local directories", name, drv)
	}
	if opts := asMap(def["driver_opts"]); len(opts) > 0 {
		s.note(Manual, "volumes", "volume %s has driver_opts %v (often a bind or NFS mount in disguise); node volumes are plain local directories", name, opts)
	}
}

func (d *deployTranslator) healthcheck(s *deployService, v any) {
	hc := asMap(v)
	if disabled, _ := hc["disable"].(bool); disabled {
		return
	}
	test := healthTest(hc["test"])
	if test == "" || test == "NONE" {
		return
	}
	timeout := healthTimeout(hc)
	if m := healthURLRE.FindStringSubmatch(test); m != nil && m[1] == "http" {
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
		s.healthProbe = &devReady{http: fmt.Sprintf("http://localhost:%d%s", p, pathPart), timeout: timeout}
		s.note(Info, "healthcheck", "`%s` imported as an HTTP probe (2xx expected); rollouts wait for it before switching traffic", test)
		return
	}
	if logLine := imageLogPattern(s.image); logLine != "" {
		s.healthProbe = &devReady{log: logLine, timeout: "120s"}
		s.note(Info, "healthcheck", "`%s` replaced by the log line %q: the node cannot exec a probe", test, logLine)
		return
	}
	if s.port > 0 {
		s.healthProbe = &devReady{tcp: fmt.Sprintf("localhost:%d", s.port), timeout: timeout}
		s.note(Manual, "healthcheck", "`%s` cannot run on a node; replaced by a TCP probe on port %d, which passes as soon as the port opens — prefer ready.http on a health endpoint, or ready.log", test, s.port)
		return
	}
	s.note(Manual, "healthcheck", "`%s` cannot run on a node and the service has no port: set ready.log to the line it prints once ready", test)
}

func (d *deployTranslator) limits(s *deployService, get func(string) (any, bool)) {
	if v, ok := get("deploy"); ok {
		dep := asMap(v)
		if r, ok := dep["replicas"]; ok {
			s.replicas = atoi(fmt.Sprint(r))
		}
		lim := asMap(asMap(dep["resources"])["limits"])
		s.cpu = str(lim["cpus"])
		s.memory = d.memory(s, str(lim["memory"]))
		for _, k := range sortedKeys(dep) {
			switch k {
			case "replicas", "resources":
			case "restart_policy", "update_config", "rollback_config":
				s.note(Info, "deploy", "%s dropped: the node restarts with backoff and rolls with soak + automatic rollback (ready.soak)", k)
			case "placement", "mode", "endpoint_mode":
				s.note(Info, "deploy", "%s dropped: a deploy.yaml runs on the node it is applied to", k)
			default:
				s.note(Info, "deploy", "%s dropped", k)
			}
		}
		if res := asMap(dep["resources"]); res["reservations"] != nil {
			s.note(Info, "deploy", "resource reservations dropped: resources: sets limits")
		}
	}
	if v, ok := get("scale"); ok && s.replicas == 0 {
		s.replicas = atoi(fmt.Sprint(v))
	}
	if v, ok := get("cpus"); ok && s.cpu == "" {
		s.cpu = str(v)
	}
	if v, ok := get("mem_limit"); ok && s.memory == "" {
		s.memory = d.memory(s, str(v))
	}
	get("memswap_limit")
}

var composeMemRE = regexp.MustCompile(`^(?i)([0-9.]+)\s*([kmgt]?)b?$`)

func (d *deployTranslator) memory(s *deployService, v string) string {
	if v == "" {
		return ""
	}
	m := composeMemRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		s.note(Manual, "resources", "memory limit %q is not understood: write it as 512Mi or 1Gi", v)
		return ""
	}
	unit := map[string]string{"": "", "k": "Ki", "m": "Mi", "g": "Gi", "t": "Ti"}[strings.ToLower(m[2])]
	return m[1] + unit
}

func (d *deployTranslator) networks(s *deployService, v any) {
	for _, name := range sortedKeys(asMap(v)) {
		cfg := asMap(asMap(v)[name])
		if aliases := strList(cfg["aliases"]); len(aliases) > 0 {
			s.note(Manual, "networks", "aliases %s do not resolve on a node, only %q does: replace those hostnames in the other services' config", strings.Join(aliases, ", "), s.name)
		}
		for _, k := range []string{"ipv4_address", "ipv6_address"} {
			if ip := str(cfg[k]); ip != "" {
				s.note(Manual, "networks", "fixed address %s is not supported: reach the service by its name %q", ip, s.name)
			}
		}
	}
}

func (d *deployTranslator) dependsOn(s *deployService, cs composeService) {
	deps := asMap(cs.raw["depends_on"])
	for _, dep := range sortedKeys(deps) {
		target := d.st.find(dep)
		if target == nil {
			s.note(Manual, "depends_on", "%s is not imported (unknown, behind a profile, or the edge proxy): drop the dependency or add the service", dep)
			continue
		}
		cfg := asMap(deps[dep])
		switch str(cfg["condition"]) {
		case "service_completed_successfully":
			if !target.job {
				target.job = true
				target.note(Info, "jobs", "moved under jobs: %s waits for it to exit 0 (condition service_completed_successfully)", s.name)
			}
		case "service_healthy":
			target.needsHealthy = true
		}
		s.dependsOn = append(s.dependsOn, dep)
	}
}

func (st *deployStack) readiness() {
	for _, s := range st.services {
		if s.job {
			if s.healthProbe != nil {
				s.note(Info, "healthcheck", "dropped: a job signals readiness by exiting 0")
			}
			if len(s.routes) > 0 {
				s.routes = nil
				s.note(Manual, "routes", "a job cannot be routed: the route was dropped")
			}
			continue
		}
		switch {
		case s.healthProbe != nil:
			s.ready = s.healthProbe
		case imageLogPattern(s.image) != "":
			s.ready = &devReady{log: imageLogPattern(s.image), timeout: "120s"}
		case s.port > 0:
			s.ready = &devReady{tcp: fmt.Sprintf("localhost:%d", s.port), timeout: "60s"}
			msg := "no healthcheck: a TCP probe on port %d gates rollouts, and it passes as soon as the port opens — set ready.http to a health endpoint"
			if s.needsHealthy {
				s.note(Manual, "ready", msg, s.port)
			} else {
				s.note(Info, "ready", msg, s.port)
			}
		}
	}
}

func (st *deployStack) backupEnv(s *deployService) {
	s.env["SKLP_BACKUP_TO"] = st.ref("BACKUP_S3_URL", "", false)
	s.env["AWS_ACCESS_KEY_ID"] = st.ref("BACKUP_S3_ACCESS_KEY", "", true)
	s.env["AWS_SECRET_ACCESS_KEY"] = st.ref("BACKUP_S3_SECRET_KEY", "", true)
	s.env["AWS_DEFAULT_REGION"] = st.ref("BACKUP_S3_REGION", "", false)
}

func isMySQL(image string) bool {
	return imageLogPattern(image) == "ready for connections"
}

func (st *deployStack) backups() {
	for _, s := range st.services {
		if s.job || len(s.volumes) == 0 {
			continue
		}
		if isPostgres(s.image) {
			user, db := "postgres", ""
			if _, ok := s.env["POSTGRES_USER"]; ok {
				user = `"$POSTGRES_USER"`
			}
			if _, ok := s.env["POSTGRES_DB"]; ok {
				db = ` "$POSTGRES_DB"`
			} else if user != "postgres" {
				db = ` "$POSTGRES_USER"`
			}
			s.backup = []string{"sh", "-c", "exec pg_dump -Fc -U " + user + db}
			st.backupEnv(s)
			s.note(Info, "backup", "a nightly pg_dump is streamed to BACKUP_S3_URL (https://<endpoint>/<bucket>/<prefix>/); the source had none")
			continue
		}
		if isMySQL(s.image) {
			if _, ok := s.env["MYSQL_ROOT_PASSWORD"]; ok {
				s.backup = []string{"sh", "-c", `exec "$(command -v mariadb-dump || command -v mysqldump)" --single-transaction --all-databases -uroot -p"$MYSQL_ROOT_PASSWORD"`}
				st.backupEnv(s)
				s.note(Info, "backup", "a nightly dump of every database is streamed to BACKUP_S3_URL (https://<endpoint>/<bucket>/<prefix>/); the source had none")
				continue
			}
		}
		var names []string
		for _, v := range s.volumes {
			if n := strings.SplitN(v, ":", 2)[0]; !containsString(s.scratch, n) {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			continue
		}
		s.note(Manual, "backup", "volume(s) %s hold this service's data and nothing backs them up: add a backup: block (a command writing an archive to stdout + a cron schedule) and SKLP_BACKUP_TO in env", strings.Join(names, ", "))
	}
}

func (d *deployTranslator) topLevel() {
	for _, k := range sortedKeys(d.proj.topLevel) {
		switch {
		case k == "version" || k == "name" || k == "networks" || k == "volumes" || strings.HasPrefix(k, "x-"):
		case k == "include":
			d.stackNote(Manual, "include", "included compose files %v are not imported: re-run on a merged file (`docker compose config > compose.yaml`) or add their services by hand", d.proj.topLevel[k])
		case k == "secrets" || k == "configs":
			d.stackNote(Manual, k, "top-level %s are not supported: pass values as `${VAR}` secrets in env, bake files into the image (`COPY`)", k)
		default:
			d.stackNote(Manual, k, "top-level compose key with no deploy.yaml equivalent")
		}
	}
}

var secretNameRE = regexp.MustCompile(`(?i)(PASS|SECRET|TOKEN|KEY|CREDENTIAL|PRIVATE|SALT|AUTH|DSN)`)

func isPostgres(image string) bool {
	return imageLogPattern(image) == "database system is ready to accept connections"
}

func (d *deployTranslator) ref(name, def string, secret bool) string {
	r := d.st.ref(name, def, secret)
	if d.vars.local[name] {
		d.st.vars[name].local = true
	}
	return r
}

func (st *deployStack) ref(name, def string, secret bool) string {
	v := st.vars[name]
	if v == nil {
		v = &deployVar{name: name}
		st.vars[name] = v
	}
	if secret || secretNameRE.MatchString(name) {
		v.secret = true
	}
	if def != "" && !v.hasDef {
		v.def, v.hasDef = def, true
	}
	return "${" + name + "}"
}

var composeRefRE = regexp.MustCompile(`\$\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])((?:[^{}]|\{[^{}]*\})*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// convert rewrites compose interpolation into deploy.yaml's: every reference
// becomes `${NAME}`, resolved at deploy time, except a non-secret default,
// which is inlined. deploy.yaml has no `$$` escape and expands every braced
// name, so `$${X}` (compose's "let the container expand it") becomes `$X`.
func (d *deployTranslator) convert(s noter, field, key, value string) string {
	if !strings.Contains(value, "$") {
		return value
	}
	where := field
	if key != "" {
		where = field + " " + key
	}
	return composeRefRE.ReplaceAllStringFunc(value, func(match string) string {
		m := composeRefRE.FindStringSubmatch(match)
		switch {
		case m[1] != "":
			return "$" + m[1]
		case match == "$$":
			return "$"
		case m[5] != "":
			return d.resolve(s, where, m[5], "", "")
		}
		return d.resolve(s, where, m[2], m[3], m[4])
	})
}

func (d *deployTranslator) resolve(s noter, where, name, op, def string) string {
	secret := secretNameRE.MatchString(name)
	switch strings.TrimPrefix(op, ":") {
	case "+":
		s.note(Manual, "environment", "%s: compose used %q only when %s is set; deploy.yaml has no such operator — write the value it should have", where, def, name)
		return d.ref(name, "", false)
	case "-":
		if strings.Contains(def, "$") {
			return d.ref(name, "", false)
		}
		if secret || d.vars.local[name] {
			return d.ref(name, def, false)
		}
		return def
	case "?":
		return d.ref(name, "", false)
	}
	if tv, ok := d.vars.template[name]; ok && !secret && !d.vars.local[name] && tv != "" {
		s.note(Info, "environment", "%s: %s inlined from %s", where, name, d.vars.templateFrom[name])
		return tv
	}
	return d.ref(name, "", false)
}

func (st *deployStack) varsNote() {
	if len(st.vars) == 0 {
		return
	}
	names := make([]string, 0, len(st.vars))
	for n := range st.vars {
		names = append(names, n)
	}
	sort.Strings(names)
	var secrets, plain []string
	for _, n := range names {
		v := st.vars[n]
		label := n
		switch {
		case v.hasDef && v.secret:
			label += " (the source wrote a value for it; never reuse that one in production)"
		case v.hasDef:
			label += fmt.Sprintf(" (default %q)", v.def)
		case v.file != "":
			label += " (read from " + v.file + ")"
		case v.local:
			label += " (your .env sets it)"
		}
		if v.secret {
			secrets = append(secrets, label)
		} else {
			plain = append(plain, label)
		}
	}
	var parts []string
	if len(secrets) > 0 {
		parts = append(parts, fmt.Sprintf("secrets — `sklp config set -s %s -secret NAME value`: %s", st.name, strings.Join(secrets, ", ")))
	}
	if len(plain) > 0 {
		parts = append(parts, fmt.Sprintf("settings — `sklp config set -s %s NAME value`: %s", st.name, strings.Join(plain, ", ")))
	}
	st.stackNote(Manual, "variables", "sklp deploy refuses to start while one of these is unset (a git reconcile reads them from the Skalpai project secrets). %s", strings.Join(parts, "; "))
}

func renderDeploy(st *deployStack, sources []string) []byte {
	root := mapping()
	header := []string{
		"Imported for production from " + strings.Join(sources, " + ") + " by `sklp init -from " + st.origin + " -to deploy`.",
		fmt.Sprintf("%d manual action(s) left: grep TODO(import). Lines marked import: record a translation choice.", ManualCount(st.allNotes())),
	}
	for _, n := range st.notes {
		header = append(header, n.comment())
	}
	put(root, "version", scalarInt(1))
	root.Content[0].HeadComment = strings.Join(header, "\n")
	put(root, "kind", scalar("deploy"))
	put(root, "name", scalar(st.name))
	if len(st.envFiles) > 0 {
		put(root, "env_files", seq(st.envFiles))
	}
	if len(st.registries) > 0 {
		regs := mapping()
		for _, r := range st.registries {
			rm := mapping()
			put(rm, "username", scalar(r.username))
			put(rm, "password", scalar(r.password))
			put(regs, r.host, rm)
		}
		put(root, "registries", regs)
	}
	if st.hasRoutes() {
		gw := mapping()
		put(gw, "email", scalar("${ACME_EMAIL}"))
		put(root, "gateway", gw)
	}
	services, jobs := mapping(), mapping()
	for _, s := range st.services {
		key := scalar(s.name)
		var comments []string
		for _, n := range s.notes {
			comments = append(comments, n.comment())
		}
		key.HeadComment = strings.Join(comments, "\n")
		target := services
		if s.job {
			target = jobs
		}
		target.Content = append(target.Content, key, renderDeployService(s))
	}
	if len(services.Content) > 0 {
		put(root, "services", services)
	}
	if len(jobs.Content) > 0 {
		put(root, "jobs", jobs)
	}
	if st.hasRoutes() {
		routes := &yaml.Node{Kind: yaml.SequenceNode}
		for _, s := range st.services {
			for _, r := range s.routes {
				rm := mapping()
				put(rm, "service", scalar(s.name))
				put(rm, "host", scalar(r.host))
				if r.path != "" {
					put(rm, "path", scalar(r.path))
				}
				if r.securityHeaders {
					put(rm, "security_headers", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
				}
				if r.compress {
					put(rm, "compress", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
				}
				routes.Content = append(routes.Content, rm)
			}
		}
		gw := mapping()
		put(gw, "routes", routes)
		space := mapping()
		put(space, "gateway", gw)
		spaces := mapping()
		put(spaces, st.space, space)
		put(root, "spaces", spaces)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}})
	_ = enc.Close()
	return spaceDeploy(buf.Bytes())
}

func spaceDeploy(out []byte) []byte {
	lines := strings.Split(string(out), "\n")
	var b strings.Builder
	section := ""
	for i, line := range lines {
		prevComment := i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#")
		if len(line) > 0 && line[0] != ' ' && line[0] != '#' {
			section, _, _ = strings.Cut(line, ":")
			if i > 0 && !prevComment && section != "kind" && section != "name" {
				b.WriteString("\n")
			}
		} else if (section == "services" || section == "jobs") && len(line) > 2 && line[:2] == "  " && line[2] != ' ' && !prevComment && !strings.HasPrefix(lines[i-1], section+":") {
			b.WriteString("\n")
		} else if (section == "services" || section == "jobs") && strings.HasPrefix(line, "  #") && i > 0 && !prevComment && !strings.HasPrefix(lines[i-1], section+":") {
			b.WriteString("\n")
		}
		b.WriteString(line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

func renderDeployService(s *deployService) *yaml.Node {
	m := mapping()
	put(m, "image", scalar(s.image))
	if len(s.command) > 0 {
		put(m, "command", flowSeq(s.command))
	}
	if s.replicas > 1 {
		put(m, "replicas", scalarInt(s.replicas))
	}
	if s.schedule != "" {
		put(m, "schedule", stringScalar(s.schedule))
	}
	if s.port > 0 {
		put(m, "port", scalarInt(s.port))
	}
	if len(s.ports) > 0 {
		put(m, "ports", seq(s.ports))
	}
	if len(s.env) > 0 {
		env := mapping()
		for _, k := range sortedStringKeys(s.env) {
			env.Content = append(env.Content, scalar(k), stringScalar(s.env[k]))
		}
		put(m, "env", env)
	}
	if len(s.volumes) > 0 {
		put(m, "volumes", seq(s.volumes))
	}
	if len(s.dependsOn) > 0 {
		put(m, "depends_on", flowSeq(s.dependsOn))
	}
	if len(s.capabilities) > 0 {
		put(m, "capabilities", flowSeq(s.capabilities))
	}
	if s.cpu != "" || s.memory != "" {
		res := mapping()
		if s.cpu != "" {
			put(res, "cpu", stringScalar(s.cpu))
		}
		if s.memory != "" {
			put(res, "memory", stringScalar(s.memory))
		}
		put(m, "resources", res)
	}
	if r := s.ready; r != nil {
		rm := mapping()
		for _, kv := range [][2]string{{"http", r.http}, {"tcp", r.tcp}, {"log", r.log}, {"timeout", r.timeout}} {
			if kv[1] != "" {
				rm.Content = append(rm.Content, scalar(kv[0]), stringScalar(kv[1]))
			}
		}
		put(m, "ready", rm)
	}
	if len(s.backup) > 0 {
		b := mapping()
		put(b, "schedule", stringScalar("0 3 * * *"))
		put(b, "command", flowSeq(s.backup))
		put(m, "backup", b)
	}
	return m
}
