package control

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"slices"
	"sort"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"strings"
)

// serviceNameRE is the strict whitelist for a space service name: a DNS/label
// shape (lowercase alphanumerics and dashes, starting alphanumeric). It bounds
// the value before it reaches the spacenet DNS table and any path derivation, so
// a name like "../../tmp/x" can never traverse or poison a downstream lookup.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validServiceName rejects any service name outside the strict DNS-label shape.
// Defence-in-depth: bundle/traefik paths are sanitised downstream, but the DNS
// name table stores the raw name, so we gate it centrally at the apply edge.
func validServiceName(name string) error {
	if !serviceNameRE.MatchString(name) {
		return fmt.Errorf("invalid service name %q: must match %s", name, serviceNameRE.String())
	}
	return nil
}

// applySpec is the decoded POST /v1/apply payload: the resolved desired manifest
// of a space. It lives in this GATED file so the embedded-only deploy types
// never leak into protocol.go / client.go (which carry the spec as opaque JSON).
type applySpec struct {
	Services []spacemanager.Service `json:"services"`
	Meta     spacemanager.SpaceMeta `json:"meta"`
}

// MarshalApplySpec encodes a resolved manifest into the opaque JSON the client
// posts as ApplyRequest.Spec. The CLI (embedded-only) builds it from Resolve's
// output; the non-gated client forwards the bytes without knowing their shape.
func MarshalApplySpec(services []spacemanager.Service, meta spacemanager.SpaceMeta) ([]byte, error) {
	return json.Marshal(applySpec{Services: services, Meta: meta})
}

// handleApply decodes the desired manifest, diffs it against the supervisor's
// live desired set, and applies the minimal mutations (add / reconfigure /
// remove) so the running supervisor converges. It returns unchanged when the
// diff is empty, else configured with the list of changes. Caller holds the
// server's apply mutex so the diff+mutate runs as a unit.
//
// space is the space this Supervisor owns (passed down from newMux), used
// only to load the persisted manifest's current owner for the cross-stack
// guard below — the apply itself always targets sup regardless of space.
func handleApply(space string, sup Supervisor, body io.Reader) (ApplyResult, error) {
	var req ApplyRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return ApplyResult{}, fmt.Errorf("decode request: %w", err)
	}
	var spec applySpec
	if req.Source != nil {
		services, meta, err := spacemanager.ResolveSource([]byte(req.Source.YAML), req.Source.Vars, space, req.Source.Stack)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("resolve deploy source: %w", err)
		}
		spec = applySpec{Services: services, Meta: meta}
	} else if err := json.Unmarshal(req.Spec, &spec); err != nil {
		return ApplyResult{}, fmt.Errorf("decode spec: %w", err)
	}
	// Cross-stack ownership guard: mirrors sklet's local TryReapply
	// (apps/sklp/sklet/reapply.go), which only ever ran on the Unix-socket
	// path. A remote `sklp deploy --server` reached this handler directly and
	// had no equivalent check, so it silently reconciled away a live space's
	// services whenever two different stacks happened to target the same
	// space name. Best-effort by design, same as the local guard: a missing
	// or unreadable manifest (first deploy, or one predating the Stack field)
	// is "unknown owner" and never blocks.
	if !req.Force {
		if owner, ok := liveSpaceOwner(space); ok && spec.Meta.Stack != "" && owner != spec.Meta.Stack {
			return ApplyResult{}, fmt.Errorf(
				"space %q is already deployed by stack %q; deploying stack %q would reconcile away its services (pass --force to adopt it)",
				space, owner, spec.Meta.Stack)
		}
	}
	for i := range spec.Services {
		spec.Services[i].DeclaredImage = spec.Services[i].Image
	}
	keepRolledImages(sup, spec.Services)
	res, err := applyDesired(sup, spec.Services, spec.Meta)
	if err == nil {
		collectImagesAfterRoll()
	}
	return res, err
}

// keepRolledImages merges an incoming manifest with what an image watch rolled
// since the last one: a service whose declared image did not change in Git
// keeps the image it runs, while a tag Git did change wins. Without the merge
// a manifest-only push would roll every watched service back to the tag it was
// first deployed with.
func keepRolledImages(sup Supervisor, services []spacemanager.Service) {
	for i := range services {
		cur, ok := sup.DesiredService(services[i].Name)
		if !ok || cur.DeclaredImage == "" {
			continue
		}
		if services[i].Image == cur.DeclaredImage || slices.Contains(cur.RolledImages, services[i].Image) {
			services[i].Image = cur.Image
			services[i].RolledImages = cur.RolledImages
		}
	}
}

// liveSpaceOwner returns the stack name recorded in the persisted manifest
// for space, read directly (no env resolution — LoadManifest does more work
// than this guard needs and that work has side effects on os.Setenv this
// caller must not trigger). ok is false when no manifest exists, it cannot be
// read/decoded, or it predates the Stack field (empty) — every such case is
// "unknown owner", matching the local guard's same fail-open behaviour.
func liveSpaceOwner(space string) (stack string, ok bool) {
	data, err := os.ReadFile(ManifestPath(space))
	if err != nil {
		return "", false
	}
	var spec applySpec
	if err := json.Unmarshal(data, &spec); err != nil || spec.Meta.Stack == "" {
		return "", false
	}
	return spec.Meta.Stack, true
}

// applyDesired reconciles the supervisor toward the given desired set and
// persists it. It is the shared body of every route that changes what a space
// runs — a full apply and a single-service image roll differ only in how they
// obtain the desired set, never in how it is applied.
func applyDesired(sup Supervisor, services []spacemanager.Service, metaIn spacemanager.SpaceMeta) (ApplyResult, error) {
	// The front-door mode is a NODE-owned property: neither a remote deploy.yaml
	// (GatewaySpec dropped the field) nor a stale client can set it. Overwrite
	// with this supervisor's own resolved mode unconditionally, so whichever app
	// deploys first can no longer decide the node's permanent front door — the
	// bug this guard exists to close. handleImage's caller already reads meta
	// from the on-disk manifest this same value produced, so this is a no-op
	// there; handleApply's caller may carry a client-supplied value, so this is
	// where it gets corrected.
	metaIn.GatewayMode = sup.GatewayMode()
	spec := applySpec{Services: services, Meta: metaIn}
	if len(spec.Services) == 0 {
		return ApplyResult{}, fmt.Errorf("manifest declares no services")
	}
	// Validate every incoming name BEFORE any mutation so an invalid one rejects
	// the whole apply (no partial application of a manifest with a bad name).
	imagePolicy := spacemanager.ImagePolicyFromEnv()
	for i := range spec.Services {
		spacemanager.ApplyDefaultResources(&spec.Services[i].Resources)
	}
	for _, svc := range spec.Services {
		if err := validServiceName(svc.Name); err != nil {
			return ApplyResult{}, err
		}
		if err := imagePolicy.Check(svc.Image); err != nil {
			return ApplyResult{}, fmt.Errorf("service %q: %w", svc.Name, err)
		}
		for _, v := range svc.Volumes {
			if err := space.ValidateNamedVolume(v); err != nil {
				return ApplyResult{}, fmt.Errorf("service %q: %w", svc.Name, err)
			}
		}
		for _, c := range svc.Capabilities {
			if err := spacemanager.ValidateManifestCapability(c); err != nil {
				return ApplyResult{}, fmt.Errorf("service %q: %w", svc.Name, err)
			}
		}
	}

	for _, r := range spec.Meta.Routes {
		if err := spacemanager.ValidateResolvedRoute(r); err != nil {
			return ApplyResult{}, err
		}
	}

	// Refresh the launcher's registry credentials from the incoming manifest
	// BEFORE any service is (re)started: a remote apply ships the resolved
	// password in spec.Meta.Registries (the agent's own env has none), so this is
	// what lets a newly-added private-image service pull. Empty auths are a no-op.
	sup.SetRegistryAuths(spacemanager.ResolveRegistryAuths(spec.Meta.Registries))

	// Desired (incoming) keyed by name; live (current) as a set.
	desired := make(map[string]spacemanager.Service, len(spec.Services))
	desiredOrder := make([]string, 0, len(spec.Services))
	for _, svc := range spec.Services {
		if _, dup := desired[svc.Name]; !dup {
			desiredOrder = append(desiredOrder, svc.Name)
		}
		desired[svc.Name] = svc
	}
	live := make(map[string]struct{})
	for _, name := range sup.DesiredServiceNames() {
		live[name] = struct{}{}
	}
	// An empty live set before this apply means the supervisor had no desired
	// services yet: this apply CREATES the space (first deploy), reported as
	// created rather than configured.
	wasEmpty := len(live) == 0

	var changes, rejections []string
	slices.SortStableFunc(desiredOrder, func(x, y string) int {
		return jobsFirst(desired[x]) - jobsFirst(desired[y])
	})
	for _, name := range desiredOrder {
		svc := desired[name]
		if _, ok := live[name]; ok {
			// A spec the supervisor just rolled back is refused, not silently
			// "unchanged": the caller must learn nothing runs it, and the
			// manifest must keep naming what does.
			if reason, rejected := sup.RejectedSpec(svc); rejected {
				rejections = append(rejections, "rejected service "+name+": "+reason)
				if cur, ok := sup.DesiredService(name); ok {
					desired[name] = cur
				}
				continue
			}
			// Only report (and only mutate-as-a-change) when the desired spec
			// actually differs — this is what makes a re-apply of an identical
			// manifest report "unchanged". Reconfigure itself is idempotent, but
			// the agent owns the comparison, so we ask it first.
			if sup.ServiceDiffers(svc) {
				sup.Reconfigure(svc)
				changes = append(changes, "reconfigured service "+name)
				continue
			}
			// The declaration is bookkeeping the merge reads next time; recording
			// it must reach the persisted manifest, so it counts as a change even
			// though nothing runs differently.
			if cur, ok := sup.DesiredService(name); ok && svc.DeclaredImage != "" && cur.DeclaredImage != svc.DeclaredImage {
				sup.SetDeclaredImage(name, svc.DeclaredImage)
				changes = append(changes, "recorded declared image for "+name)
			}
			continue
		}
		sup.AddService(svc)
		changes = append(changes, "added service "+name)
	}
	// Removed services: live but no longer desired.
	removed := make([]string, 0)
	for name := range live {
		if _, ok := desired[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	for _, name := range removed {
		sup.RemoveService(name)
		changes = append(changes, "removed service "+name)
	}

	// Converge the gateway routes AFTER the service mutations, so a newly-added
	// service already exists when its route is (re)written, and a removed one is
	// gone. SetRoutes reports whether the set actually moved; an unchanged set is
	// a no-op (identical vendor files, no traefik reload) so an identical
	// re-apply still reads "unchanged".
	if sup.SetRoutes(spec.Meta.Routes) {
		changes = append(changes, "updated gateway routes")
	}

	if len(changes) == 0 {
		// Warnings hold even when nothing changed: a database nothing backs up is
		// still a database nothing backs up.
		if len(rejections) > 0 {
			return ApplyResult{Result: ResultRejected, Changes: rejections, Warnings: applyWarnings(spec.Services)}, nil
		}
		return ApplyResult{Result: ResultUnchanged, Warnings: applyWarnings(spec.Services)}, nil
	}
	if len(rejections) > 0 {
		for i := range spec.Services {
			spec.Services[i] = desired[spec.Services[i].Name]
		}
	}

	// Persist the desired state so an agent restart resurrects this workload.
	// runDeployAgent calls LoadManifest at boot precisely to "adopt an existing
	// manifest ... rather than sitting empty", but until now only the LOCAL
	// deploy path wrote one: a space deployed over --server left nothing on
	// disk, so a restarted agent came up with zero services and the workload
	// stayed down until someone re-ran `sklp deploy` by hand.
	//
	// Secrets stay off disk. spec.Meta.Registries carries the RESOLVED registry
	// password on the remote path (the detached agent cannot read the operator's
	// env), and that value is transport-only — persistManifest strips it, so the
	// on-disk manifest keeps the name-only shape documented on RegistryAuthMeta.
	//
	// Best-effort: a persistence failure must not fail an apply that already
	// mutated the live supervisor. The workload is running; only the ability to
	// resurrect it after a restart is lost, which is worth a loud log rather
	// than an error the caller would read as "deploy failed".
	// Persist the values this apply RESOLVED before stripping them, so a restart
	// can resolve them again. Without it the manifest survives a reboot but its
	// secrets do not, and the space comes back half-running: a private image
	// cannot be pulled, a fresh Postgres cannot initialise.
	persistMu.Lock()
	if err := snapshotGeneration(spec.Meta.Name); err != nil {
		log.Printf("apply: could not keep the previous generation (a rollback will persist with the new env refs): %v", err)
	}
	if err := persistSpaceEnv(spec.Meta.Name, resolvedSecrets(spec), spec.Meta.EnvValues != nil); err != nil {
		log.Printf("apply: could not persist resolved secrets (a restart will not be able to pull private images): %v", err)
	}
	if err := persistManifest(spec); err != nil {
		log.Printf("apply: could not persist manifest (agent restart will come up empty): %v", err)
	}
	persistMu.Unlock()

	warnings := applyWarnings(spec.Services)
	if len(rejections) > 0 {
		return ApplyResult{Result: ResultRejected, Changes: append(changes, rejections...), Warnings: warnings}, nil
	}
	if wasEmpty {
		return ApplyResult{Result: ResultCreated, Changes: changes, Warnings: warnings}, nil
	}
	return ApplyResult{Result: ResultConfigured, Changes: changes, Warnings: warnings}, nil
}

// persistManifest writes the applied spec to disk with every secret stripped:
// registry passwords, and each service env value that came from a ${VAR} (the
// name is kept, the value re-resolved at boot by ResolveEnvRefs). The copy is
// shallow-then-explicit: Registries and the service slice are rebuilt so
// clearing a value cannot alias back into the caller's spec, which is still in
// use by the live supervisor (SetRegistryAuths ran above and needs the resolved
// values in memory).
func persistManifest(spec applySpec) error {
	name := spec.Meta.Name
	if name == "" {
		return fmt.Errorf("manifest has no space name")
	}
	meta := spec.Meta
	meta.EnvValues = nil
	services := spacemanager.StripEnvRefs(spec.Services, meta.EnvRefs)
	if len(meta.Registries) > 0 {
		safe := make(map[string]spacemanager.RegistryAuthMeta, len(meta.Registries))
		for host, auth := range meta.Registries {
			auth.Password = "" // transport-only; PasswordEnv is what the local path re-reads
			safe[host] = auth
		}
		meta.Registries = safe
	}
	return SaveManifest(name, services, meta)
}

// resolvedSecrets collects the values this apply resolved that the manifest is
// about to drop: every service env whose value came from a ${VAR}, plus each
// registry password the client shipped over the wire.
//
// It reads the SPEC, not the environment: on a remote apply the supervisor's own
// env is empty of these, which is the whole reason the space could not come back.
func resolvedSecrets(spec applySpec) map[string]string {
	out := map[string]string{}
	for name, value := range spec.Meta.EnvValues {
		out[name] = value
	}
	for _, svc := range spec.Services {
		for _, entry := range svc.Env {
			key, value, found := strings.Cut(entry, "=")
			if !found || value == "" {
				continue
			}
			// Keyed by the var NAME, not the env key: ExpandTemplate rebuilds
			// the value by name at boot. A client that predates EnvValues
			// ships only the resolved spec, which still carries a whole-value
			// ref's secret verbatim; a composed template cannot be split, so
			// it depends on EnvValues.
			ref, isRef := spacemanager.LookupEnvRef(spec.Meta.EnvRefs, svc.Name, key)
			if !isRef {
				continue
			}
			names := spacemanager.TemplateVarNames(ref)
			switch {
			case !spacemanager.IsEnvTemplate(ref):
				out[ref] = value
			case len(names) == 1 && ref == "${"+names[0]+"}":
				if _, shipped := out[names[0]]; !shipped {
					out[names[0]] = value
				}
			}
		}
	}
	for _, auth := range spec.Meta.Registries {
		if auth.PasswordEnv != "" && auth.Password != "" {
			out[auth.PasswordEnv] = auth.Password
		}
	}
	return out
}

// applyWarnings reports what the apply accepted but the operator should know
// about while they are still watching — today, named volumes no `backup:`
// covers. Surfaced here as well as at space start because an operator reads the
// apply output and rarely the node's journal.
func applyWarnings(services []spacemanager.Service) []string {
	unbacked := spacemanager.UnbackedVolumes(services)
	if len(unbacked) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("no backup declared for %s — this data cannot be restored if the node is lost",
		strings.Join(unbacked, ", "))}
}

// jobsFirst orders an apply so a changed job is queued before the services
// that depend on it begin their rollout gate check.
func jobsFirst(svc spacemanager.Service) int {
	if svc.Oneshot {
		return 0
	}
	return 1
}
