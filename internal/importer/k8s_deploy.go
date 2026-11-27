package importer

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type K8sOptions struct {
	Root    string
	Stack   string
	Sources []string
}

// ImportK8sDeploy translates Kubernetes manifests (kustomizations, manifest
// directories or files) into a production `.sklp/<stack>/deploy.yaml`. With
// no Sources it looks for them under Root, and returns nil when none exist.
func ImportK8sDeploy(opts K8sOptions) (*ComposeImport, error) {
	sources := opts.Sources
	if len(sources) == 0 {
		sources = FindK8sSources(opts.Root)
	}
	if len(sources) == 0 {
		return nil, nil
	}
	objs, err := loadK8s(opts.Root, sources)
	if err != nil {
		return nil, err
	}
	if opts.Stack == "" {
		opts.Stack = "<stack>"
	}
	project := slug(filepath.Base(absOr(opts.Root)))
	k := &k8sTranslator{
		st:   &deployStack{origin: "k8s", name: opts.Stack, space: project + "-production", project: project, vars: map[string]*deployVar{}},
		objs: objs,
	}
	k.translate()
	return &ComposeImport{Sources: sources, YAML: renderDeploy(k.st, sources), Notes: k.st.allNotes()}, nil
}

type k8sService struct {
	obj      k8sObject
	deploy   string
	ports    map[int]int
	selected bool
}

type pgCluster struct {
	name, service, owner, db, passwordVar, secret string
}

type k8sTranslator struct {
	st         *deployStack
	objs       []k8sObject
	configMaps map[string]map[string]string
	secrets    map[string]map[string]string
	secretKeys map[string][]string
	services   map[string]*k8sService
	clusters   map[string]*pgCluster
	middleware map[string]map[string]any
	pullHosts  map[string]bool
	consumed   map[string]bool
}

func (k *k8sTranslator) key(ns, name string) string { return ns + "/" + name }

func (k *k8sTranslator) translate() {
	k.configMaps, k.secrets, k.secretKeys = map[string]map[string]string{}, map[string]map[string]string{}, map[string][]string{}
	k.services, k.clusters, k.middleware = map[string]*k8sService{}, map[string]*pgCluster{}, map[string]map[string]any{}
	k.pullHosts, k.consumed = map[string]bool{}, map[string]bool{}
	sort.SliceStable(k.objs, func(i, j int) bool {
		a, b := k.objs[i], k.objs[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		return a.name < b.name
	})
	k.index()
	for _, o := range k.objs {
		if o.group() == "postgresql.cnpg.io" && o.kind == "Cluster" {
			k.consumed[o.ref()] = true
			k.st.services = append(k.st.services, k.cnpgCluster(o))
		}
	}
	for _, o := range k.objs {
		switch o.kind {
		case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet":
			k.consumed[o.ref()] = true
			k.workload(o, asMap(o.spec()["template"]), false, "")
		case "Job":
			if o.group() == "batch" {
				k.consumed[o.ref()] = true
				k.workload(o, asMap(o.spec()["template"]), true, "")
			}
		case "CronJob":
			if o.group() == "batch" {
				k.consumed[o.ref()] = true
				spec := o.spec()
				k.workload(o, asMap(asMap(asMap(spec["jobTemplate"])["spec"])["template"]), true, str(spec["schedule"]))
			}
		case "Pod":
			k.consumed[o.ref()] = true
			k.workload(o, map[string]any{"metadata": o.raw["metadata"], "spec": o.raw["spec"]}, false, "")
		}
	}
	k.exposedServices()
	for _, o := range k.objs {
		switch {
		case o.kind == "Ingress" && o.group() == "networking.k8s.io":
			k.consumed[o.ref()] = true
			k.ingress(o)
		case o.kind == "IngressRoute" && strings.HasPrefix(o.group(), "traefik."):
			k.consumed[o.ref()] = true
			k.ingressRoute(o)
		case o.kind == "HTTPRoute" && o.group() == "gateway.networking.k8s.io":
			k.consumed[o.ref()] = true
			k.httpRoute(o)
		}
	}
	k.rewriteAddresses()
	k.inferDependencies()
	k.st.readiness()
	k.st.backups()
	k.registries()
	k.leftovers()
	k.st.finish()
}

func (k *k8sTranslator) index() {
	for _, o := range k.objs {
		switch {
		case o.kind == "ConfigMap" && o.apiVersion == "v1":
			data := map[string]string{}
			for key, v := range asMap(o.raw["data"]) {
				data[key] = str(v)
			}
			k.configMaps[k.key(o.namespace, o.name)] = data
		case o.kind == "Secret" && o.apiVersion == "v1":
			data := map[string]string{}
			for key, v := range asMap(o.raw["data"]) {
				if dec, err := base64.StdEncoding.DecodeString(str(v)); err == nil {
					data[key] = string(dec)
				}
			}
			for key, v := range asMap(o.raw["stringData"]) {
				data[key] = str(v)
			}
			k.secrets[k.key(o.namespace, o.name)] = data
			k.secretKeys[k.key(o.namespace, o.name)] = sortedKeysOf(data)
		case o.kind == "ExternalSecret" && o.group() == "external-secrets.io":
			target := str(asMap(o.spec()["target"])["name"])
			if target == "" {
				target = o.name
			}
			var keys, origins []string
			for _, d := range anyList(o.spec()["data"]) {
				dm := asMap(d)
				keys = append(keys, str(dm["secretKey"]))
				origins = append(origins, str(dm["secretKey"])+" ← "+str(asMap(dm["remoteRef"])["key"]))
			}
			if o.spec()["dataFrom"] == nil {
				k.secretKeys[k.key(o.namespace, target)] = keys
			}
			if len(origins) > 0 {
				k.st.stackNote(Info, "secrets", "ExternalSecret %s read from store %s: %s — copy each value into the matching variable", o.name, str(asMap(o.spec()["secretStoreRef"])["name"]), strings.Join(origins, ", "))
			}
		case o.kind == "SealedSecret":
			var keys []string
			for key := range asMap(o.spec()["encryptedData"]) {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			k.secretKeys[k.key(o.namespace, o.name)] = keys
		case o.kind == "Service" && o.apiVersion == "v1":
			k.services[k.key(o.namespace, o.name)] = &k8sService{obj: o, ports: map[int]int{}}
		case o.kind == "Middleware" && strings.HasPrefix(o.group(), "traefik."):
			k.middleware[k.key(o.namespace, o.name)] = o.spec()
		}
	}
}

func sortedKeysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (k *k8sTranslator) selectingService(ns string, labels map[string]any) *k8sService {
	var found []*k8sService
	for _, ks := range k.services {
		if ks.obj.namespace != ns {
			continue
		}
		sel := asMap(ks.obj.spec()["selector"])
		if len(sel) == 0 {
			continue
		}
		match := true
		for key, v := range sel {
			if str(labels[key]) != str(v) {
				match = false
				break
			}
		}
		if match {
			found = append(found, ks)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].obj.name < found[j].obj.name })
	for _, ks := range found {
		if !ks.selected {
			return ks
		}
	}
	return nil
}

func (k *k8sTranslator) workload(o k8sObject, template map[string]any, job bool, schedule string) {
	podSpec := asMap(template["spec"])
	labels := asMap(asMap(template["metadata"])["labels"])
	containers := anyList(podSpec["containers"])
	if len(containers) == 0 {
		k.st.stackNote(Manual, "services", "%s has no container: nothing imported", o.ref())
		return
	}
	name := o.name
	ks := (*k8sService)(nil)
	if !job {
		ks = k.selectingService(o.namespace, labels)
	}
	if ks != nil {
		ks.selected = true
		name = ks.obj.name
	}
	volumes := map[string]map[string]any{}
	for _, v := range anyList(podSpec["volumes"]) {
		vm := asMap(v)
		volumes[str(vm["name"])] = vm
	}
	if o.kind == "StatefulSet" {
		for _, t := range anyList(o.spec()["volumeClaimTemplates"]) {
			tn := str(asMap(asMap(t)["metadata"])["name"])
			volumes[tn] = map[string]any{"name": tn, "volumeClaimTemplate": true}
		}
	}

	if drivesKubeAPI(asMap(containers[0])) {
		k.st.stackNote(Info, "services", "%s drives the Kubernetes API (kubectl), which a space does not have: dropped", o.ref())
		return
	}
	var inits, waited []string
	for _, ic := range anyList(podSpec["initContainers"]) {
		c := asMap(ic)
		if p, _ := c["restartPolicy"].(string); p == "Always" {
			containers = append(containers, ic)
			continue
		}
		if host, ok := waitLoopTarget(c); ok {
			waited = append(waited, host)
			continue
		}
		in := k.container(o, name+"-"+str(c["name"]), c, podSpec, volumes, true)
		in.note(Info, "jobs", "was an init container of %s: it now runs once as a job %s waits for", o.ref(), name)
		k.st.services = append(k.st.services, in)
		inits = append(inits, in.name)
	}

	main := k.container(o, name, asMap(containers[0]), podSpec, volumes, job)
	main.schedule = schedule
	main.dependsOn = append(main.dependsOn, inits...)
	main.waitsFor = waited
	if ks != nil {
		k.servicePorts(ks, main, asMap(containers[0]))
	}
	k.workloadFields(o, main, podSpec, job)
	k.st.services = append(k.st.services, main)

	for _, extra := range containers[1:] {
		c := asMap(extra)
		side := k.container(o, name+"-"+str(c["name"]), c, podSpec, volumes, job)
		side.note(Manual, "", "was a container of the %s pod, sharing localhost with %s: inside the space it is a separate service — replace localhost:<port> between them with <service>:<port>, and give it the volumes they shared", o.ref(), name)
		k.st.services = append(k.st.services, side)
	}
}

func (k *k8sTranslator) workloadFields(o k8sObject, s *deployService, podSpec map[string]any, job bool) {
	spec := o.spec()
	switch o.kind {
	case "Deployment", "StatefulSet", "ReplicaSet":
		if r, ok := spec["replicas"]; ok {
			s.replicas = atoi(str(r))
		}
		if o.kind == "StatefulSet" && s.replicas > 1 {
			s.note(Manual, "replicas", "%d StatefulSet replicas each had their own volume and stable identity; here every replica shares the same volumes — keep 1 unless the service is stateless", s.replicas)
		}
	case "DaemonSet":
		s.note(Info, "", "was a DaemonSet: it runs once, on the node this deploy is applied to")
	case "CronJob":
		if tz := str(spec["timeZone"]); tz != "" {
			s.note(Manual, "schedule", "the CronJob ran in time zone %s; the node fires schedules in its own clock — shift the hours if they differ", tz)
		}
		if str(spec["suspend"]) == "true" {
			s.note(Manual, "schedule", "the CronJob was suspended (a manual trigger); remove the job if it should not run on this schedule")
		}
	}
	if job && o.kind == "Job" {
		s.note(Info, "jobs", "was a Job: it runs once per apply")
	}
	if len(anyList(podSpec["imagePullSecrets"])) > 0 {
		if host := registryHost(s.image); host != "" {
			k.pullHosts[host] = true
		}
	}
	for _, key := range []string{"nodeSelector", "affinity", "tolerations", "topologySpreadConstraints", "priorityClassName"} {
		if podSpec[key] != nil {
			s.note(Info, "", "%s dropped: one node runs the whole deploy", key)
		}
	}
	if podSpec["hostNetwork"] == true || podSpec["hostPID"] == true || podSpec["hostIPC"] == true {
		s.note(Manual, "", "host namespaces (hostNetwork/hostPID/hostIPC) are not available to a space service")
	}
}

var (
	waitLoopRE  = regexp.MustCompile(`(?s)until\b.*\b(?:nc\s+-z|pg_isready\s+-h|nslookup|wget\s+-q[^ ]*|curl\s+-[^ ]*)\s+(?:https?://)?([A-Za-z0-9.-]+)`)
	kubeAPIRE   = regexp.MustCompile(`\bkubectl\b`)
	kubectlImgs = []string{"bitnami/kubectl", "alpine/k8s", "rancher/kubectl", "lachlanevenson/k8s-kubectl"}
)

// waitLoopTarget spots the init container that only polls another service
// until it answers: a space expresses that with depends_on + ready.
func waitLoopTarget(c map[string]any) (string, bool) {
	script := strings.Join(append(strList(c["command"]), strList(c["args"])...), " ")
	m := waitLoopRE.FindStringSubmatch(script)
	if m == nil {
		return "", false
	}
	host, _, _ := strings.Cut(m[1], ".")
	return host, true
}

func drivesKubeAPI(c map[string]any) bool {
	image := str(c["image"])
	for _, img := range kubectlImgs {
		if strings.Contains(image, img) {
			return true
		}
	}
	return kubeAPIRE.MatchString(strings.Join(append(strList(c["command"]), strList(c["args"])...), " "))
}

func registryHost(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if ok && (strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost") {
		return first
	}
	return ""
}

func (k *k8sTranslator) container(o k8sObject, name string, c, podSpec map[string]any, volumes map[string]map[string]any, job bool) *deployService {
	s := &deployService{name: name, env: map[string]string{}, job: job}
	s.image = str(c["image"])
	switch tag := imageTag(s.image); {
	case s.image == "":
		s.image = placeholderImage(k.st.project, name)
		s.note(Manual, "image", "the container has no image: set the one it runs")
	case tag == "" || tag == "latest":
		s.note(Manual, "image", "%s is not pinned: production must name an exact version (a rollout only changes what the file says)", s.image)
	}
	for _, p := range anyList(c["ports"]) {
		pm := asMap(p)
		if s.port == 0 && (str(pm["protocol"]) == "" || str(pm["protocol"]) == "TCP") {
			s.port = atoi(str(pm["containerPort"]))
		}
	}
	envOrder := k.env(o, s, c)
	command := strList(c["command"])
	args := strList(c["args"])
	if len(command) > 0 {
		s.note(Manual, "command", "Kubernetes command: replaced the image ENTRYPOINT; deploy.yaml's command: is appended to the ENTRYPOINT instead — it is kept as command+args, which only works when the image has no ENTRYPOINT")
	}
	for _, a := range append(command, args...) {
		s.command = append(s.command, k.expandDependent(s, "command", a, envOrder))
	}
	k.probe(s, c)
	k.resources(s, asMap(c["resources"]))
	k.security(s, asMap(c["securityContext"]), asMap(podSpec["securityContext"]))
	k.mounts(s, c, volumes)
	for _, key := range []string{"lifecycle", "workingDir", "stdin", "tty"} {
		if c[key] != nil {
			switch key {
			case "lifecycle":
				s.note(Manual, "lifecycle", "postStart/preStop hooks are not supported: fold them into the command, or tune ready.drain for a graceful stop")
			case "workingDir":
				s.note(Manual, "workingDir", "deploy.yaml has no working directory: the image WORKDIR applies (%s was set)", str(c[key]))
			}
		}
	}
	return s
}

var dependentRE = regexp.MustCompile(`\$\$\(|\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)

// expandDependent resolves Kubernetes `$(VAR)` references against the
// container's own env, which is what the kubelet does, and turns literal
// `${X}` into `$X` since deploy.yaml expands every braced name itself.
func (k *k8sTranslator) expandDependent(s *deployService, field, v string, env map[string]string) string {
	if !strings.Contains(v, "$") {
		return v
	}
	v = bracedLiteralRE.ReplaceAllString(v, "$$$1")
	return dependentRE.ReplaceAllStringFunc(v, func(m string) string {
		if m == "$$(" {
			return "$("
		}
		name := m[2 : len(m)-1]
		if val, ok := env[name]; ok {
			return val
		}
		s.note(Manual, field, "$(%s) does not name an env var of this container: Kubernetes left it as is; write the value it should have", name)
		return m
	})
}

var bracedLiteralRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func (k *k8sTranslator) env(o k8sObject, s *deployService, c map[string]any) map[string]string {
	resolved := map[string]string{}
	for _, ef := range anyList(c["envFrom"]) {
		e := asMap(ef)
		prefix := str(e["prefix"])
		if cm := asMap(e["configMapRef"]); cm != nil {
			data, ok := k.configMaps[k.key(o.namespace, str(cm["name"]))]
			if !ok {
				s.note(Manual, "envFrom", "ConfigMap %s is not in the manifests: list its keys under env:", str(cm["name"]))
				continue
			}
			for _, key := range sortedKeysOf(data) {
				s.env[prefix+key] = k.literal(s, data[key])
				resolved[prefix+key] = s.env[prefix+key]
			}
			s.note(Info, "envFrom", "ConfigMap %s inlined", str(cm["name"]))
		}
		if sr := asMap(e["secretRef"]); sr != nil {
			keys, ok := k.secretKeys[k.key(o.namespace, str(sr["name"]))]
			if !ok {
				s.note(Manual, "envFrom", "the keys of secret %s are not in the manifests (absent, or filled by an ExternalSecret dataFrom): add each variable it provides as `KEY: ${KEY}`", str(sr["name"]))
				continue
			}
			for _, key := range keys {
				s.env[prefix+key] = k.secretRef(s, o.namespace, str(sr["name"]), key, prefix+key)
				resolved[prefix+key] = s.env[prefix+key]
			}
			s.note(Info, "envFrom", "every key of secret %s is passed as ${KEY}", str(sr["name"]))
		}
	}
	for _, ev := range anyList(c["env"]) {
		e := asMap(ev)
		name := str(e["name"])
		if v, ok := e["value"]; ok {
			s.env[name] = k.envValue(s, name, str(v), resolved)
			resolved[name] = s.env[name]
			continue
		}
		from := asMap(e["valueFrom"])
		switch {
		case from["secretKeyRef"] != nil:
			ref := asMap(from["secretKeyRef"])
			s.env[name] = k.secretRef(s, o.namespace, str(ref["name"]), str(ref["key"]), name)
			if ref["optional"] == true {
				s.note(Info, "env", "%s was optional; sklp deploy requires every ${VAR} — set it to an empty value if unused", name)
			}
		case from["configMapKeyRef"] != nil:
			ref := asMap(from["configMapKeyRef"])
			data, ok := k.configMaps[k.key(o.namespace, str(ref["name"]))]
			val, has := data[str(ref["key"])]
			if ok && has {
				s.env[name] = k.literal(s, val)
			} else {
				s.env[name] = k.st.ref(name, "", false)
				s.note(Info, "env", "%s came from ConfigMap %s, which is not in the manifests: it is now a setting", name, str(ref["name"]))
			}
		case from["fieldRef"] != nil:
			path := str(asMap(from["fieldRef"])["fieldPath"])
			switch path {
			case "metadata.name":
				s.env[name] = s.name
				s.note(Info, "env", "%s (the pod name) is now the service name", name)
			case "metadata.namespace":
				s.env[name] = o.namespace
				s.note(Info, "env", "%s (the namespace) inlined as %q", name, o.namespace)
			default:
				s.note(Manual, "env", "%s read %s from the pod: there is no downward API — set the value it needs", name, path)
			}
		case from["resourceFieldRef"] != nil:
			s.note(Manual, "env", "%s read a resource limit from the pod: write the value", name)
		}
		if v, ok := s.env[name]; ok {
			resolved[name] = v
		}
	}
	return resolved
}

func (k *k8sTranslator) literal(s *deployService, v string) string {
	if !strings.Contains(v, "${") {
		return v
	}
	return bracedLiteralRE.ReplaceAllString(v, "$$$1")
}

func (k *k8sTranslator) envValue(s *deployService, name, v string, resolved map[string]string) string {
	if secretNameRE.MatchString(name) && v != "" && !strings.Contains(v, "$(") {
		s.note(Info, "env", "%s was written in clear in the manifest: it is now ${%s}, a secret to set per deployment", name, name)
		return k.st.ref(name, v, true)
	}
	return k.expandDependent(s, "env", v, resolved)
}

func (k *k8sTranslator) secretRef(s *deployService, ns, secret, key, envName string) string {
	for _, cl := range k.clusters {
		if secret == cl.secret || secret == cl.name+"-app" {
			if v, ok := cl.secretValue(key, k); ok {
				return v
			}
		}
	}
	if data, ok := k.secrets[k.key(ns, secret)]; ok {
		if val, has := data[key]; has {
			return k.st.ref(envName, val, true)
		}
	}
	if len(k.clusters) == 1 && databaseURLRE.MatchString(envName) {
		for _, cl := range k.clusters {
			uri, _ := cl.secretValue("uri", k)
			s.note(Info, "env", "%s came from secret %s; with one postgres cluster in the manifests it is assumed to point at it: %s", envName, secret, uri)
			return uri
		}
	}
	return k.st.ref(envName, "", true)
}

var databaseURLRE = regexp.MustCompile(`^(DATABASE_URL|DB_URL|POSTGRES_URL|PG_URL|DATABASE_DSN|POSTGRES_DSN)$`)

func (cl *pgCluster) secretValue(key string, k *k8sTranslator) (string, bool) {
	pw := k.st.ref(cl.passwordVar, "", true)
	switch key {
	case "password":
		return pw, true
	case "username", "user":
		return cl.owner, true
	case "dbname":
		return cl.db, true
	case "host":
		return cl.service, true
	case "port":
		return "5432", true
	case "uri":
		return fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", cl.owner, pw, cl.service, cl.db), true
	}
	return "", false
}

func (k *k8sTranslator) probe(s *deployService, c map[string]any) {
	var probe map[string]any
	var which string
	for _, key := range []string{"readinessProbe", "startupProbe", "livenessProbe"} {
		if p := asMap(c[key]); p != nil {
			probe, which = p, key
			break
		}
	}
	if probe == nil {
		return
	}
	period, failures, delay := atoiOr(probe["periodSeconds"], 10), atoiOr(probe["failureThreshold"], 3), atoiOr(probe["initialDelaySeconds"], 0)
	total := delay + period*failures
	if total < 30 {
		total = 30
	}
	if total > 300 {
		total = 300
	}
	timeout := strconv.Itoa(total) + "s"
	switch {
	case probe["httpGet"] != nil:
		h := asMap(probe["httpGet"])
		port := k.containerPort(c, h["port"])
		p := str(h["path"])
		if p == "" {
			p = "/"
		}
		if strings.EqualFold(str(h["scheme"]), "HTTPS") {
			s.healthProbe = &devReady{tcp: fmt.Sprintf("localhost:%d", port), timeout: timeout}
			s.note(Manual, "ready", "the %s probed HTTPS; the node probes plain HTTP, so it became a TCP probe on %d", which, port)
			return
		}
		s.healthProbe = &devReady{http: fmt.Sprintf("http://localhost:%d%s", port, p), timeout: timeout}
		s.note(Info, "ready", "taken from the %s", which)
	case probe["tcpSocket"] != nil:
		port := k.containerPort(c, asMap(probe["tcpSocket"])["port"])
		s.healthProbe = &devReady{tcp: fmt.Sprintf("localhost:%d", port), timeout: timeout}
		s.note(Info, "ready", "taken from the %s", which)
	case probe["exec"] != nil:
		cmd := shellJoin(strList(asMap(probe["exec"])["command"]))
		if logLine := imageLogPattern(s.image); logLine != "" {
			s.healthProbe = &devReady{log: logLine, timeout: "120s"}
			s.note(Info, "ready", "the %s `%s` became the log line %q: the node cannot exec a probe", which, cmd, logLine)
			return
		}
		s.note(Manual, "ready", "the %s ran `%s`, which a node cannot exec: set ready.http, ready.tcp or ready.log", which, cmd)
	case probe["grpc"] != nil:
		port := atoiOr(asMap(probe["grpc"])["port"], s.port)
		s.healthProbe = &devReady{tcp: fmt.Sprintf("localhost:%d", port), timeout: timeout}
		s.note(Info, "ready", "the gRPC %s became a TCP probe on %d", which, port)
	}
}

func atoiOr(v any, def int) int {
	if v == nil {
		return def
	}
	if n := atoi(str(v)); n > 0 {
		return n
	}
	return def
}

func (k *k8sTranslator) containerPort(c map[string]any, port any) int {
	if n := atoi(str(port)); n > 0 {
		return n
	}
	for _, p := range anyList(c["ports"]) {
		pm := asMap(p)
		if str(pm["name"]) == str(port) {
			return atoi(str(pm["containerPort"]))
		}
	}
	return 0
}

func (k *k8sTranslator) resources(s *deployService, r map[string]any) {
	lim, req := asMap(r["limits"]), asMap(r["requests"])
	s.cpu, s.memory = str(lim["cpu"]), str(lim["memory"])
	used := false
	if s.cpu == "" && req["cpu"] != nil {
		s.cpu, used = str(req["cpu"]), true
	}
	if s.memory == "" && req["memory"] != nil {
		s.memory, used = str(req["memory"]), true
	}
	if used {
		s.note(Info, "resources", "no limit was set: the request is used as the limit")
	}
}

func (k *k8sTranslator) security(s *deployService, c, pod map[string]any) {
	if c["privileged"] == true {
		s.note(Manual, "securityContext", "privileged containers are refused by sklp: find which capability the service really needs")
	}
	if caps := strList(asMap(c["capabilities"])["add"]); len(caps) > 0 {
		s.capabilities = caps
		s.note(Info, "securityContext", "capabilities %s copied; the node's admission policy refuses the dangerous ones at apply", strings.Join(caps, ", "))
	}
	for _, m := range []map[string]any{c, pod} {
		if m["runAsUser"] != nil || m["runAsGroup"] != nil || m["fsGroup"] != nil {
			s.note(Info, "securityContext", "runAsUser/runAsGroup/fsGroup dropped: the service runs as the image's user inside a rootless user namespace")
			return
		}
	}
}

func (k *k8sTranslator) mounts(s *deployService, c map[string]any, volumes map[string]map[string]any) {
	for _, vm := range anyList(c["volumeMounts"]) {
		m := asMap(vm)
		name, dst := str(m["name"]), str(m["mountPath"])
		suffix := ""
		if m["readOnly"] == true {
			suffix = ":ro"
		}
		if m["subPath"] != nil {
			s.note(Info, "volumes", "subPath %s on %s dropped: the whole volume is mounted", str(m["subPath"]), dst)
		}
		v := volumes[name]
		switch {
		case v == nil:
			s.note(Manual, "volumes", "volume %s mounted on %s is not declared in the pod", name, dst)
		case v["volumeClaimTemplate"] == true:
			vol := s.name + "-" + name
			s.volumes = append(s.volumes, vol+":"+dst+suffix)
			s.note(Info, "volumes", "volumeClaimTemplate %s is the named volume %s; the PVC data is not copied", name, vol)
		case v["persistentVolumeClaim"] != nil:
			claim := str(asMap(v["persistentVolumeClaim"])["claimName"])
			s.volumes = append(s.volumes, claim+":"+dst+suffix)
			s.note(Info, "volumes", "PVC %s is the named volume %s; its data is not copied — restore it before the first start", claim, claim)
		case v["emptyDir"] != nil:
			vol := s.name + "-" + name
			s.volumes = append(s.volumes, vol+":"+dst+suffix)
			s.scratch = append(s.scratch, vol)
			s.note(Info, "volumes", "emptyDir %s is the named volume %s: unlike the emptyDir it survives a restart", name, vol)
		case v["configMap"] != nil || v["secret"] != nil || v["projected"] != nil:
			s.note(Manual, "volumes", "%s mounts files from a ConfigMap/Secret on %s: bake the files into the image (`COPY`) or pass the values through env", name, dst)
		case v["hostPath"] != nil:
			s.note(Manual, "volumes", "hostPath %s on %s: a space only mounts named volumes", str(asMap(v["hostPath"])["path"]), dst)
		default:
			s.note(Manual, "volumes", "volume %s on %s has a source with no deploy.yaml equivalent", name, dst)
		}
	}
}

func (k *k8sTranslator) servicePorts(ks *k8sService, s *deployService, c map[string]any) {
	ks.deploy = s.name
	for _, p := range anyList(ks.obj.spec()["ports"]) {
		pm := asMap(p)
		port := atoi(str(pm["port"]))
		target := port
		if t, ok := pm["targetPort"]; ok {
			target = k.containerPort(c, t)
		}
		ks.ports[port] = target
		if s.port == 0 {
			s.port = target
		}
		if port != target {
			s.note(Info, "port", "Service %s listened on %d and forwarded to %d: inside the space it is reached at %s:%d, and the addresses in env were rewritten", ks.obj.name, port, target, s.name, target)
		}
		switch str(ks.obj.spec()["type"]) {
		case "LoadBalancer", "NodePort":
			pub := port
			if np := atoi(str(pm["nodePort"])); np > 0 && str(ks.obj.spec()["type"]) == "NodePort" {
				pub = np
			}
			if tcpOnlyPorts[target] || strings.EqualFold(str(pm["protocol"]), "UDP") || !httpLike(pm, target) {
				proto := ""
				if strings.EqualFold(str(pm["protocol"]), "UDP") {
					proto = "/udp"
				}
				s.ports = append(s.ports, fmt.Sprintf("0.0.0.0:%d:%d%s", pub, target, proto))
				s.note(Info, "ports", "the %s Service published %d: it is bound on every host address", str(ks.obj.spec()["type"]), pub)
			} else {
				s.addRoute(target)
				s.note(Manual, "routes", "the %s Service exposed HTTP port %d: it becomes a gateway route — set its public hostname under spaces:", str(ks.obj.spec()["type"]), port)
			}
		}
	}
}

func httpLike(pm map[string]any, target int) bool {
	name := strings.ToLower(str(pm["name"]))
	return strings.Contains(name, "http") || strings.Contains(name, "web") || target == 80 || target == 443 || target == 8080 || target == 3000
}

func (k *k8sTranslator) exposedServices() {
	for _, key := range sortedKeysAny(k.services) {
		ks := k.services[key]
		k.consumed[ks.obj.ref()] = true
		if ks.deploy != "" {
			continue
		}
		if str(ks.obj.spec()["type"]) == "ExternalName" {
			k.st.stackNote(Manual, "services", "Service %s is an ExternalName alias for %s: point the clients at that address directly", ks.obj.name, str(ks.obj.spec()["externalName"]))
			continue
		}
		if len(asMap(ks.obj.spec()["selector"])) == 0 {
			continue
		}
		k.st.stackNote(Manual, "services", "Service %s selects no imported workload (its pods come from an operator or a chart): add the service it fronts by hand", ks.obj.name)
	}
}

func sortedKeysAny[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (k *k8sTranslator) deployFor(ns, svc string, port int) (*deployService, bool) {
	ks := k.services[k.key(ns, svc)]
	name := svc
	if ks != nil && ks.deploy != "" {
		name = ks.deploy
	}
	for _, cl := range k.clusters {
		if svc == cl.name+"-rw" || svc == cl.name+"-ro" || svc == cl.name+"-r" {
			name = cl.service
		}
	}
	s := k.st.find(name)
	if s == nil || s.job {
		return nil, false
	}
	target := port
	if ks != nil {
		if t, ok := ks.ports[port]; ok {
			target = t
		}
	}
	return s, target == 0 || s.port == 0 || target == s.port
}

func (k *k8sTranslator) addRoute(where string, ns, svc string, port int, r deployRoute) {
	s, samePort := k.deployFor(ns, svc, port)
	if s == nil {
		k.st.stackNote(Manual, "routes", "%s routes %s%s to %s, which is not an imported service: add the route by hand", where, r.host, r.path, svc)
		return
	}
	if !samePort {
		s.note(Manual, "routes", "%s sends %s to port %d, but a route always reaches the service's port: (%d) — split the service or align the ports", where, r.host, port, s.port)
	}
	if r.path == "/" {
		r.path = ""
	}
	for i, existing := range s.routes {
		if existing.host == r.host && existing.path == r.path {
			s.routes[i].compress = existing.compress || r.compress
			s.routes[i].securityHeaders = existing.securityHeaders || r.securityHeaders
			return
		}
	}
	if len(s.routes) == 1 && strings.HasSuffix(s.routes[0].host, ".example.com") {
		s.routes = nil
		s.notes = dropNotes(s.notes, "routes")
	}
	s.routes = append(s.routes, r)
}

func (k *k8sTranslator) ingress(o k8sObject) {
	spec := o.spec()
	ann := asMap(asMap(o.raw["metadata"])["annotations"])
	base := deployRoute{}
	for _, mw := range strings.Split(str(ann["traefik.ingress.kubernetes.io/router.middlewares"]), ",") {
		mw = strings.TrimSuffix(strings.TrimSpace(mw), "@kubernetescrd")
		if mw == "" {
			continue
		}
		ns, name := k.splitNamespaced(mw)
		if k.applyMiddleware(&base, ns, name) {
			return
		}
	}
	if ann["nginx.ingress.kubernetes.io/rewrite-target"] != nil {
		k.st.stackNote(Manual, "routes", "Ingress %s rewrites paths (rewrite-target): the gateway forwards paths unchanged — make the service serve them", o.name)
	}
	if spec["tls"] != nil {
		k.st.stackNote(Info, "routes", "Ingress %s TLS secrets dropped: the gateway obtains certificates itself", o.name)
	}
	if def := asMap(spec["defaultBackend"]); def != nil {
		k.st.stackNote(Manual, "routes", "Ingress %s has a default backend: routes need a host, add one", o.name)
	}
	for _, rule := range anyList(spec["rules"]) {
		rm := asMap(rule)
		host := str(rm["host"])
		if host == "" {
			k.st.stackNote(Manual, "routes", "Ingress %s has a rule without host: routes need one", o.name)
			continue
		}
		for _, p := range anyList(asMap(rm["http"])["paths"]) {
			pm := asMap(p)
			svc := asMap(asMap(pm["backend"])["service"])
			port := atoi(str(asMap(svc["port"])["number"]))
			if port == 0 {
				port = k.namedServicePort(o.namespace, str(svc["name"]), str(asMap(svc["port"])["name"]))
			}
			r := base
			r.host, r.path = host, str(pm["path"])
			k.addRoute("Ingress "+o.name, o.namespace, str(svc["name"]), port, r)
		}
	}
}

func (k *k8sTranslator) namedServicePort(ns, svc, name string) int {
	ks := k.services[k.key(ns, svc)]
	if ks == nil {
		return 0
	}
	for _, p := range anyList(ks.obj.spec()["ports"]) {
		if str(asMap(p)["name"]) == name {
			return atoi(str(asMap(p)["port"]))
		}
	}
	return 0
}

// splitNamespaced splits traefik's `<namespace>-<name>` middleware reference,
// where both halves may themselves contain dashes.
func (k *k8sTranslator) splitNamespaced(ref string) (string, string) {
	for _, key := range sortedKeysAny(k.middleware) {
		ns, name, _ := strings.Cut(key, "/")
		if ref == ns+"-"+name {
			return ns, name
		}
	}
	ns, name, _ := strings.Cut(ref, "-")
	return ns, name
}

func (k *k8sTranslator) applyMiddleware(r *deployRoute, ns, name string) bool {
	mw, ok := k.middleware[k.key(ns, name)]
	if !ok {
		for key, spec := range k.middleware {
			if strings.HasSuffix(key, "/"+name) {
				mw, ok = spec, true
			}
		}
	}
	if !ok {
		return false
	}
	switch {
	case mw["redirectScheme"] != nil:
		return true
	case mw["compress"] != nil:
		r.compress = true
	case mw["headers"] != nil:
		r.securityHeaders = true
	}
	return false
}

func (k *k8sTranslator) ingressRoute(o k8sObject) {
	for _, route := range anyList(o.spec()["routes"]) {
		rm := asMap(route)
		r := deployRoute{}
		redirect := false
		for _, mw := range anyList(rm["middlewares"]) {
			m := asMap(mw)
			ns := str(m["namespace"])
			if ns == "" {
				ns = o.namespace
			}
			if k.applyMiddleware(&r, ns, str(m["name"])) {
				redirect = true
			}
		}
		if redirect {
			continue
		}
		match := str(rm["match"])
		if m := pathMatchRE.FindStringSubmatch(match); m != nil {
			r.path = m[1]
		}
		services := anyList(rm["services"])
		if len(services) == 0 {
			continue
		}
		svc := asMap(services[0])
		for _, hm := range hostMatchRE.FindAllStringSubmatch(match, -1) {
			for _, q := range quotedRE.FindAllStringSubmatch(hm[1], -1) {
				rr := r
				rr.host = q[1]
				k.addRoute("IngressRoute "+o.name, o.namespace, str(svc["name"]), atoi(str(svc["port"])), rr)
			}
		}
	}
}

func (k *k8sTranslator) httpRoute(o k8sObject) {
	spec := o.spec()
	hosts := strList(spec["hostnames"])
	if len(hosts) == 0 {
		k.st.stackNote(Manual, "routes", "HTTPRoute %s has no hostnames: routes need one", o.name)
		return
	}
	for _, rule := range anyList(spec["rules"]) {
		rm := asMap(rule)
		backends := anyList(rm["backendRefs"])
		if len(backends) == 0 {
			continue
		}
		b := asMap(backends[0])
		paths := []string{""}
		if ms := anyList(rm["matches"]); len(ms) > 0 {
			paths = nil
			for _, m := range ms {
				paths = append(paths, str(asMap(asMap(m)["path"])["value"]))
			}
		}
		for _, h := range hosts {
			for _, p := range paths {
				k.addRoute("HTTPRoute "+o.name, o.namespace, str(b["name"]), atoi(str(b["port"])), deployRoute{host: h, path: p})
			}
		}
	}
}

var pgMajorTagRE = regexp.MustCompile(`:(\d+)`)

func (k *k8sTranslator) cnpgCluster(o k8sObject) *deployService {
	spec := o.spec()
	initdb := asMap(asMap(spec["bootstrap"])["initdb"])
	cl := &pgCluster{name: o.name, service: o.name + "-rw", owner: str(initdb["owner"]), db: str(initdb["database"]), secret: str(asMap(initdb["secret"])["name"])}
	if cl.db == "" {
		cl.db = "app"
	}
	if cl.owner == "" {
		cl.owner = cl.db
	}
	cl.passwordVar = "POSTGRES_PASSWORD"
	if len(k.clusters) > 0 {
		cl.passwordVar = strings.ToUpper(strings.ReplaceAll(o.name, "-", "_")) + "_PASSWORD"
	}
	k.clusters[o.name] = cl
	major := "17"
	imageName := str(spec["imageName"])
	if m := pgMajorTagRE.FindStringSubmatch(imageName); m != nil {
		major = m[1]
	}
	s := &deployService{name: cl.service, image: "postgres:" + major, port: 5432, env: map[string]string{}}
	s.env["POSTGRES_USER"] = cl.owner
	s.env["POSTGRES_DB"] = cl.db
	s.env["POSTGRES_PASSWORD"] = k.st.ref(cl.passwordVar, "", true)
	dataDir := "/var/lib/postgresql/data"
	if atoi(major) >= 18 {
		dataDir = "/var/lib/postgresql"
	}
	s.volumes = []string{o.name + "-data:" + dataDir}
	s.note(Info, "", "CloudNativePG Cluster %s became the official postgres:%s image, named after its read-write Service %s; the %s-ro and %s-r addresses are rewritten to it", o.name, major, cl.service, o.name, o.name)
	if imageName != "" && !strings.Contains(imageName, "cloudnative-pg/postgresql") {
		s.note(Manual, "image", "the cluster ran %s: pick the official image carrying the same extensions (postgis/postgis, pgvector/pgvector, timescale/timescaledb)", imageName)
	}
	if n := atoi(str(spec["instances"])); n > 1 {
		s.note(Info, "", "%d instances became one: a node runs a single postgres, without replicas", n)
	}
	if size := str(asMap(spec["storage"])["size"]); size != "" {
		s.note(Info, "volumes", "the cluster's %s volume data is not copied: pg_dump it from the cluster and restore it before switching traffic", size)
	}
	if params := asMap(asMap(spec["postgresql"])["parameters"]); len(params) > 0 {
		s.command = []string{"postgres"}
		for _, key := range sortedKeys(params) {
			s.command = append(s.command, "-c", key+"="+str(params[key]))
		}
	}
	k.resources(s, asMap(spec["resources"]))
	if spec["plugins"] != nil || spec["backup"] != nil {
		s.note(Info, "backup", "the cluster's WAL archiving to object storage is replaced by the nightly pg_dump below")
	}
	if asMap(spec["managed"])["roles"] != nil {
		s.note(Manual, "", "managed roles are not created: add them with an init job running psql")
	}
	return s
}

var addressRE = regexp.MustCompile(`(^|[^A-Za-z0-9.-])([a-z0-9]([a-z0-9-]*[a-z0-9])?)((\.[a-z0-9-]+)(\.svc(\.cluster\.local)?)?)?(:(\d+))?`)

// rewriteAddresses turns the in-cluster addresses services used to reach
// each other (`svc`, `svc.ns`, `svc.ns.svc.cluster.local`, a Service port)
// into the space's: the deploy service name and the container port.
func (k *k8sTranslator) rewriteAddresses() {
	type target struct {
		name  string
		ports map[int]int
	}
	known := map[string]target{}
	for _, ks := range k.services {
		if ks.deploy != "" {
			known[ks.obj.name] = target{ks.deploy, ks.ports}
		}
	}
	for _, cl := range k.clusters {
		for _, n := range []string{cl.name + "-rw", cl.name + "-ro", cl.name + "-r"} {
			known[n] = target{cl.service, map[int]int{5432: 5432}}
		}
	}
	namespaces := map[string]bool{}
	for _, o := range k.objs {
		if o.namespace != "" {
			namespaces[o.namespace] = true
		}
	}
	rewrite := func(v string) string {
		return addressRE.ReplaceAllStringFunc(v, func(m string) string {
			p := addressRE.FindStringSubmatch(m)
			host, nsPart, svcPart, portStr := p[2], p[5], p[6], p[9]
			t, ok := known[host]
			if !ok {
				return m
			}
			hasNS := nsPart != "" && namespaces[strings.TrimPrefix(nsPart, ".")]
			if nsPart != "" && !hasNS {
				return m
			}
			if !hasNS && svcPart == "" && portStr == "" && t.name == host {
				return m
			}
			out := p[1] + t.name
			if portStr != "" {
				port := atoi(portStr)
				if tp, ok := t.ports[port]; ok && tp > 0 {
					port = tp
				}
				out += ":" + strconv.Itoa(port)
			}
			return out
		})
	}
	for _, s := range k.st.services {
		for _, key := range sortedStringKeys(s.env) {
			v := rewrite(s.env[key])
			s.env[key] = v
			if m := clusterAddrRE.FindString(v); m != "" {
				s.note(Manual, "env", "%s reaches %s, a Service outside these manifests: replace it with that service's public URL", key, m)
			}
		}
		for i, a := range s.command {
			s.command[i] = rewrite(a)
		}
		for _, host := range s.waitsFor {
			if t, ok := known[host]; ok && !containsString(s.dependsOn, t.name) {
				s.dependsOn = append(s.dependsOn, t.name)
				s.note(Info, "depends_on", "an init container polled %s until it answered: now depends_on, gated by its ready probe", t.name)
			}
		}
	}
}

var clusterAddrRE = regexp.MustCompile(`[a-z0-9-]+\.[a-z0-9-]+\.svc(\.cluster\.local)?`)

func (k *k8sTranslator) inferDependencies() {
	for _, s := range k.st.services {
		var deps []string
		for _, other := range k.st.services {
			if other == s || other.job {
				continue
			}
			values := append([]string(nil), s.command...)
			for _, v := range s.env {
				values = append(values, v)
			}
			for _, v := range values {
				if refersTo(v, other.name) {
					deps = append(deps, other.name)
					break
				}
			}
		}
		for _, d := range deps {
			if !containsString(s.dependsOn, d) {
				s.dependsOn = append(s.dependsOn, d)
			}
		}
		if len(deps) > 0 {
			s.note(Info, "depends_on", "inferred from the addresses it calls: %s", strings.Join(deps, ", "))
		}
	}
}

func refersTo(v, name string) bool {
	for _, sep := range []string{"//", "@"} {
		if strings.Contains(v, sep+name+":") || strings.Contains(v, sep+name+"/") || strings.HasSuffix(v, sep+name) {
			return true
		}
	}
	return v == name || strings.HasPrefix(v, name+":")
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (k *k8sTranslator) registries() {
	hosts := sortedKeysAny(k.pullHosts)
	for _, h := range hosts {
		prefix := "REGISTRY"
		if len(hosts) > 1 {
			prefix = strings.ToUpper(slugRE.ReplaceAllString(h, "_")) + "_REGISTRY"
		}
		k.st.registries = append(k.st.registries, deployRegistry{host: h, username: k.st.ref(prefix+"_USERNAME", "", false), password: k.st.ref(prefix+"_PASSWORD", "", true)})
	}
	if len(hosts) > 0 {
		k.st.stackNote(Info, "registries", "imagePullSecrets became registries: credentials for %s", strings.Join(hosts, ", "))
	}
}

var droppedKinds = map[string]string{
	"Namespace":                "a deploy runs in its own space",
	"NetworkPolicy":            "a space is already isolated; use allow_egress for outbound rules",
	"GlobalNetworkPolicy":      "a space is already isolated; use allow_egress for outbound rules",
	"CiliumNetworkPolicy":      "a space is already isolated; use allow_egress for outbound rules",
	"PodDisruptionBudget":      "rollouts soak and roll back on their own",
	"HorizontalPodAutoscaler":  "replicas are fixed: set replicas: to the load you expect",
	"ServiceAccount":           "no Kubernetes API in a space",
	"Role":                     "no Kubernetes API in a space",
	"RoleBinding":              "no Kubernetes API in a space",
	"ClusterRole":              "no Kubernetes API in a space",
	"ClusterRoleBinding":       "no Kubernetes API in a space",
	"ServiceMonitor":           "the node ships metrics and logs itself",
	"PodMonitor":               "the node ships metrics and logs itself",
	"PrometheusRule":           "alerting lives in Skalpai",
	"Certificate":              "the gateway obtains certificates itself",
	"Issuer":                   "the gateway obtains certificates itself",
	"ClusterIssuer":            "the gateway obtains certificates itself",
	"Middleware":               "compress/headers became route flags, redirects are the gateway's",
	"ConfigMap":                "values were inlined where services read them",
	"Secret":                   "values are never copied: each one is a ${VAR} to set",
	"ExternalSecret":           "values are never copied: each one is a ${VAR} to set",
	"SealedSecret":             "values are never copied: each one is a ${VAR} to set",
	"SecretStore":              "secrets are set with sklp config set or as Skalpai project secrets",
	"ClusterSecretStore":       "secrets are set with sklp config set or as Skalpai project secrets",
	"PersistentVolumeClaim":    "claims became named volumes",
	"PersistentVolume":         "claims became named volumes",
	"StorageClass":             "node volumes are local directories",
	"ScheduledBackup":          "replaced by the nightly pg_dump",
	"ObjectStore":              "replaced by the nightly pg_dump",
	"Application":              "the node reconciles from git itself",
	"AppProject":               "the node reconciles from git itself",
	"LimitRange":               "limits are set per service under resources:",
	"ResourceQuota":            "limits are set per service under resources:",
	"PriorityClass":            "one node runs the whole deploy",
	"VerticalPodAutoscaler":    "set resources: by hand",
	"IngressClass":             "the gateway is the only ingress",
	"Pooler":                   "the application connects to postgres directly",
	"CustomResourceDefinition": "no Kubernetes API in a space",
}

func (k *k8sTranslator) leftovers() {
	dropped, unknown := map[string][]string{}, []string{}
	for _, o := range k.objs {
		if k.consumed[o.ref()] {
			continue
		}
		if _, ok := droppedKinds[o.kind]; ok {
			dropped[o.kind] = append(dropped[o.kind], o.name)
			continue
		}
		unknown = append(unknown, o.apiVersion+" "+o.ref())
	}
	for _, kind := range sortedKeysAny(dropped) {
		if kind == "HorizontalPodAutoscaler" {
			k.st.stackNote(Info, "kinds", "HorizontalPodAutoscaler %s dropped: %s", strings.Join(dropped[kind], ", "), droppedKinds[kind])
			continue
		}
		k.st.stackNote(Info, "kinds", "%s %s dropped: %s", kind, strings.Join(dropped[kind], ", "), droppedKinds[kind])
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		k.st.stackNote(Manual, "kinds", "no deploy.yaml equivalent for %s: check what each one does and reproduce it by hand", strings.Join(unknown, ", "))
	}
}
