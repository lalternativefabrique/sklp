// Package reconcile_deploy makes a node's deployed topology match a
// deploy.yaml declared in Git — the topology counterpart of
// apps/core/imageupdate/application/reconcile_from_git, which only tracks an
// image tag. See issue 206ec5bd for the full design and the secrets
// limitation below.
package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
	imageupdate "github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/reconcile/source"
)

// FileFetcher reads one file from a repo at a given ref, returning
// source.ErrFileNotFound when it does not exist there — same
// contract as reconcile_from_git.FileFetcher (deliberately not shared: this
// package does not import imageupdate, to keep the two contexts
// independently deployable).
type FileFetcher interface {
	FetchFileAtRef(ctx context.Context, repo, path, ref, token string) (string, error)
}

// NodeResolver opens a gateway.Node's stored bearer token — mirrors
// reconcile_from_git.NodeResolver, declared locally so this package does not
// import gateway/infrastructure.
type NodeResolver interface {
	Decrypt(blob []byte) (string, error)
}

// NodeAddress is where a declared node_id points, with the bearer token still
// sealed. Kept apart from the token so a reconcile that finds nothing changed
// never opens the secret (see reconcileOne's hash short-circuit).
type NodeAddress struct {
	ServerURL     string
	CAFingerprint string
	SealedToken   []byte
}

// NodeDirectory resolves the node a deploy-root names into an address and the
// credential to reach it.
//
// Credential takes the name the operator registered the host under — the same
// name the VM carries — so a deploy-root reads like the infrastructure it
// describes, and a node changing address does not have to be chased through
// every repo that targets it. The stackID scopes the lookup: a name a stack
// does not hold is refused, which is what keeps one tenant from deploying
// onto another's node.
//
// CredentialByID resolves the superseded node_id form, kept for the roots
// already deployed. It disappears with the last node_id in the wild.
type NodeDirectory interface {
	Credential(ctx context.Context, stackID, nodeName string) (NodeAddress, error)
	CredentialByID(ctx context.Context, stackID, nodeID string) (NodeAddress, error)
}

// ErrNodeNotFound reports a node_id no registry entry matches.
var ErrNodeNotFound = errors.New("node not found")

type Handler struct {
	repo    domain.Repository
	fetcher FileFetcher
	nodes   NodeDirectory
	decrypt NodeResolver
	applier domain.Applier
	state   domain.StateFetcher
	secrets domain.Resolver
	creds   imageupdate.RegistryCredentialRepository
	now     func() time.Time
}

// secrets may be nil — a deployment with no ${VAR} in its env parses fine
// either way, and one that does gets ErrSecretsNotSupported instead of a
// nil-pointer panic (see domain.ParseManifest's doc).
func New(repo domain.Repository, fetcher FileFetcher, nodes NodeDirectory, decrypt NodeResolver, applier domain.Applier, state domain.StateFetcher, secrets domain.Resolver) *Handler {
	return &Handler{repo: repo, fetcher: fetcher, nodes: nodes, decrypt: decrypt, applier: applier, state: state, secrets: secrets, now: time.Now}
}

// WithRegistryCredentials lets a `registries:` password that no store defines
// come from the stack's registry credentials, the ones its image watches use.
func (h *Handler) WithRegistryCredentials(creds imageupdate.RegistryCredentialRepository) *Handler {
	h.creds = creds
	return h
}

func (h *Handler) registryPassword(stackID string) func(context.Context, string) (string, bool) {
	if h.creds == nil {
		return nil
	}
	return func(ctx context.Context, imageRepo string) (string, bool) {
		all, err := h.creds.ListByStack(ctx, stackID)
		if err != nil {
			slog.Warn("deployconfig: stack registry credentials unavailable", "stack_id", stackID, "error", err)
			return "", false
		}
		c := imageupdate.MatchCredential(all, imageRepo)
		if c == nil || c.Secret == "" {
			return "", false
		}
		return c.Secret, true
	}
}

// Handle reads deploy-root.yaml at cmd.Ref and, for every deployment it
// declares whose manifest differs from the last one Skalpai applied, POSTs the
// manifest to the node the root names — creating the space on first deploy, re-applying on drift
// (POST /v1/spaces/<space>/apply is create-or-update; see issue 206ec5bd's
// transport research). Absence of the file, a malformed YAML, a ${VAR}
// secret reference, or an unresolvable node are all logged and treated as
// "nothing to reconcile" rather than a hard failure — mirroring
// reconcile_from_git's per-entry tolerance, so a problem with THIS
// deployment never blocks anything else the same push might reconcile
// (image watches, other future config).
func (h *Handler) Handle(ctx context.Context, cmd Command) error {
	for _, d := range h.plan(ctx, cmd) {
		if err := h.reconcileOne(ctx, cmd, d); err != nil {
			return err
		}
	}
	return nil
}

// plan is what this push has to reconcile: the deployments deploy-root.yaml
// declares. The root is the only source of destination, so a repo without
// one reconciles nothing.
func (h *Handler) plan(ctx context.Context, cmd Command) []domain.RootDeployment {
	raw, err := h.fetcher.FetchFileAtRef(ctx, cmd.Repo, domain.RootPath, cmd.Ref, cmd.GitToken)
	if err != nil {
		if errors.Is(err, source.ErrFileNotFound) {
			slog.Warn("deployconfig: no deploy-root.yaml, nothing to reconcile",
				"stack_id", cmd.StackID, "path", domain.RootPath)
		} else {
			slog.Warn("deployconfig: could not read deploy-root, skipping reconciliation",
				"stack_id", cmd.StackID, "path", domain.RootPath, "error", err)
		}
		return nil
	}
	root, perr := domain.ParseRoot([]byte(raw))
	if errors.Is(perr, domain.ErrNotARoot) {
		slog.Warn("deployconfig: no deploy-root.yaml, nothing to reconcile",
			"stack_id", cmd.StackID, "path", domain.RootPath, "error", perr)
		return nil
	}
	if perr != nil {
		slog.Warn("deployconfig: deploy-root could not be parsed, skipping reconciliation",
			"stack_id", cmd.StackID, "path", domain.RootPath, "error", perr)
		return nil
	}
	return root.Deployments
}

// secretsManifest reads the secrets.yaml beside this manifest, which says
// which store each ${VAR} comes from. Absent is the normal case and not an
// error: every reference then resolves from the Skalpai store, as it did
// before manifests existed. A malformed one is logged and treated the same
// way rather than failing the reconcile, so a typo in it cannot take a
// deployment down — the missing secret it causes reports itself by name.
func (h *Handler) secretsManifest(ctx context.Context, cmd Command, manifestPath string) *domain.SecretsManifest {
	raw, err := h.fetcher.FetchFileAtRef(ctx, cmd.Repo, domain.SecretsPathFor(manifestPath), cmd.Ref, cmd.GitToken)
	if err != nil {
		return nil
	}
	m, err := domain.ParseSecretsManifest([]byte(raw))
	if err != nil {
		slog.Warn("deployconfig: secrets manifest could not be parsed, falling back to the project store",
			"stack_id", cmd.StackID, "path", domain.SecretsPathFor(manifestPath), "error", err)
		return nil
	}
	return m
}

// plainConfig reads the values the config.yaml beside this manifest states in
// the clear, so a ${VAR} bound to a hostname or a model name resolves without
// being pushed into the secret store as if it were one. Absent or unreadable
// is not an error: the manifest may reference no config at all, and one that
// does fails further down naming the value it could not resolve.
func (h *Handler) plainConfig(ctx context.Context, cmd Command, manifestPath string) map[string]string {
	raw, err := h.fetcher.FetchFileAtRef(ctx, cmd.Repo, domain.ConfigPathFor(manifestPath), cmd.Ref, cmd.GitToken)
	if err != nil {
		return nil
	}
	return domain.ParsePlainConfig([]byte(raw))
}

// reconcileOne applies a single declared deployment.
func (h *Handler) reconcileOne(ctx context.Context, cmd Command, decl domain.RootDeployment) error {
	// Deployment.Name is the manifest's path, kept for display; the row is
	// identified by its space (see domain.Repository.FindBySpace), which is
	// what the node deploys and what stays unique across the repos of a stack.
	deploymentName := decl.Path
	raw, err := h.fetcher.FetchFileAtRef(ctx, cmd.Repo, decl.Path, cmd.Ref, cmd.GitToken)
	if err != nil {
		if errors.Is(err, source.ErrFileNotFound) {
			slog.Warn("deployconfig: deploy-root points at a missing manifest",
				"stack_id", cmd.StackID, "space", decl.Space, "path", decl.Path)
			return nil
		}
		return fmt.Errorf("fetch %s: %w", decl.Path, err)
	}

	manifest, err := domain.ParseManifestWith(ctx, []byte(raw), domain.ParseOptions{
		Plain:     h.plainConfig(ctx, cmd, decl.Path),
		Resolver:  h.secrets,
		Secrets:   h.secretsManifest(ctx, cmd, decl.Path),
		ProjectID: cmd.ProjectID,
		UserID:    cmd.UserID,

		RegistryPassword: h.registryPassword(cmd.StackID),
	})
	if err != nil {
		slog.Warn("deployconfig: deploy.yaml could not be parsed, skipping reconciliation",
			"stack_id", cmd.StackID, "path", decl.Path, "error", err)
		return h.recordPreApplyFailure(ctx, cmd.StackID, decl.Space, err)
	}
	manifest.Node = decl.Node
	manifest.NodeID = decl.NodeID
	manifest.Space = decl.Space

	node, err := h.credentialFor(ctx, cmd.StackID, manifest.Node, manifest.NodeID)
	if err != nil {
		slog.Warn("deployconfig: could not resolve target node",
			"stack_id", cmd.StackID, "node", manifest.Node, "node_id", manifest.NodeID, "error", err)
		return h.recordPreApplyFailure(ctx, cmd.StackID, decl.Space, err)
	}

	deployment, err := h.repo.FindBySpace(ctx, cmd.StackID, decl.Space)
	if err != nil {
		return fmt.Errorf("find existing deployment: %w", err)
	}
	isNew := deployment == nil
	if isNew {
		deployment, err = domain.NewDeployment(cmd.StackID, deploymentName, manifest.Node, manifest.NodeID, manifest.Space)
		if err != nil {
			slog.Warn("deployconfig: could not build deployment from Git",
				"stack_id", cmd.StackID, "error", err)
			return nil
		}
	} else {
		deployment.Node = manifest.Node
		deployment.NodeID = manifest.NodeID
		deployment.Space = manifest.Space
	}
	if cmd.ProjectID != "" {
		deployment.ProjectID = cmd.ProjectID
	}

	hash := manifest.Hash()

	token, err := h.decrypt.Decrypt(node.SealedToken)
	if err != nil {
		slog.Warn("deployconfig: could not decrypt node token",
			"stack_id", cmd.StackID, "node_id", manifest.NodeID, "error", err)
		return h.recordPreApplyFailure(ctx, cmd.StackID, decl.Space, err)
	}
	target := domain.NodeTarget{
		ServerURL:     node.ServerURL,
		Space:         manifest.Space,
		Token:         token,
		CAFingerprint: node.CAFingerprint,
		Stack:         domain.StackOf(decl.Path),
	}

	result, err := h.applier.Apply(ctx, target, manifest)
	if err != nil {
		deployment.RecordFailure(err.Error())
		if saveErr := h.save(ctx, deployment, isNew); saveErr != nil {
			return saveErr
		}
		slog.Warn("deployconfig: apply failed",
			"stack_id", cmd.StackID, "node_id", manifest.NodeID, "space", manifest.Space, "error", err)
		return nil
	}

	deployment.RecordApplied(hash, h.now())
	slog.Info("deployconfig: applied deploy.yaml",
		"stack_id", cmd.StackID, "node_id", manifest.NodeID, "space", manifest.Space,
		"result", result.Result, "changes", result.Changes)

	// One immediate health check, not a poll loop: this handler runs inside
	// the push-webhook request path (see contract's ConfigReconciler doc),
	// so blocking here until AllReady flips true would hold that request
	// open for however long the service takes to pull its image and pass
	// its readiness probe — seconds to minutes, unbounded from this
	// handler's perspective. A single snapshot right after the apply is
	// still strictly more information than issue 206ec5bd's step-2 starting point
	// (no check at all, ever); Ready=false here just means "not converged
	// YET" — a later reconcile of the same unchanged manifest does not
	// re-check (see the hash-match branch above), so today a deployment
	// that took a moment to become ready stays Ready=false until its NEXT
	// content change. Revisit with a background poller if that turns out
	// to matter in practice.
	h.checkHealth(ctx, cmd.StackID, target, deployment)

	return h.save(ctx, deployment, isNew)
}

// checkHealth runs the post-apply health check and folds its outcome into
// deployment via RecordHealth. A nil h.state (no StateFetcher wired) or a
// transport failure reaching the node both leave Ready at its RecordApplied
// default (false) rather than guessing — see domain.Deployment.RecordHealth's
// doc on how a check failure differs from ready=false.
func (h *Handler) checkHealth(ctx context.Context, stackID string, target domain.NodeTarget, deployment *domain.Deployment) {
	if h.state == nil {
		return
	}
	st, err := h.state.State(ctx, target)
	if err != nil {
		deployment.RecordHealth(false, err.Error(), h.now())
		slog.Warn("deployconfig: post-apply health check failed",
			"stack_id", stackID, "node_id", deployment.NodeID, "space", target.Space, "error", err)
		return
	}
	deployment.RecordHealth(st.AllReady, "", h.now())
	if !st.AllReady {
		slog.Warn("deployconfig: applied but not all services ready yet",
			"stack_id", stackID, "node_id", deployment.NodeID, "space", target.Space)
	}
}

// recordPreApplyFailure marks a deployment Skalpai already applied once when a
// reconcile dies before reaching the node — an unresolvable ${VAR}, a node that
// no longer exists, a token that will not open.
//
// Without it the row keeps the state its last successful push left: ready, no
// error, a hash matching a manifest that has since changed. The UI then reads
// as healthy while nothing has been applied, which is how a reconcile broken by
// a schema drift ran unnoticed for hours.
//
// A deployment that was never applied has no row, and inventing one would list
// something that does not exist — the logged warning stays its only trace.
// Failing to persist is not worth failing the whole reconcile over: the other
// deployments of the same push still deserve their turn.
func (h *Handler) recordPreApplyFailure(ctx context.Context, stackID, space string, cause error) error {
	deployment, err := h.repo.FindBySpace(ctx, stackID, space)
	if err != nil || deployment == nil {
		return nil
	}
	deployment.RecordFailure(cause.Error())
	if err := h.repo.Update(ctx, deployment); err != nil {
		slog.Warn("deployconfig: could not record the reconcile failure",
			"stack_id", stackID, "space", space, "error", err)
	}
	return nil
}

func (h *Handler) save(ctx context.Context, d *domain.Deployment, isNew bool) error {
	if isNew {
		return h.repo.Save(ctx, d)
	}
	return h.repo.Update(ctx, d)
}

// credentialFor hands back the credential for the node an entry names,
// preferring the operator-facing name over the superseded id.
func (h *Handler) credentialFor(ctx context.Context, stackID, nodeName, nodeID string) (NodeAddress, error) {
	if nodeName != "" {
		return h.nodes.Credential(ctx, stackID, nodeName)
	}
	return h.nodes.CredentialByID(ctx, stackID, nodeID)
}
