// Package reconcile_from_git makes Postgres a mirror of a repo's declared
// image watches, so a watch's configuration lives in Git rather than being
// retyped into a form that cannot validate it against the deploy manifest.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gopkg.in/yaml.v3"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/reconcile/source"
)

// ConfigFilePath is where a project's watches are declared, at the root of
// its GitHub repo — fixed rather than derived from the stack name, since the
// stack a project belongs to is a Skalpai concept the connected repo does not
// share.
const ConfigFilePath = domain.WatchesPath

// FileFetcher reads one file from a repo at a given ref, returning
// source.ErrFileNotFound when it does not exist there.
type FileFetcher interface {
	FetchFileAtRef(ctx context.Context, repo, path, ref, token string) (string, error)
}

// NodeResolver opens a gateway.Node's stored bearer token, mirroring
// create_watch.NodeDecrypter — declared locally so this package does not
// import gateway/infrastructure.
type NodeResolver interface {
	Decrypt(blob []byte) (string, error)
}

// NodeAddress is where a declared node_id points, with the bearer token still
// sealed. These values are COPIED into the Watch on resolution and never read
// again: a later token or address rotation does not reach an existing watch,
// which is what lets the poller run without touching the node registry.
type NodeAddress struct {
	ServerURL     string
	CAFingerprint string
	SealedToken   []byte
}

// NodeDirectory hands over the credential for the node a watch names, and
// only when resolving a declaration — never on the poll path, which reads the
// copy taken here.
//
// Credential takes the name the operator registered the host under, so a
// watch reads like the infrastructure it targets and survives that node
// changing address. The stackID scopes it, which is what stops a repo from
// watching a service on another tenant's node.
//
// CredentialByID resolves the superseded node_id form, kept for the files
// already committed.
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
}

func New(repo domain.Repository, fetcher FileFetcher, nodes NodeDirectory, decrypt NodeResolver) *Handler {
	return &Handler{repo: repo, fetcher: fetcher, nodes: nodes, decrypt: decrypt}
}

type watchFile struct {
	Watches []watchEntry `yaml:"watches"`
}

type watchEntry struct {
	Name       string `yaml:"name"`
	Service    string `yaml:"service"`
	ImageRepo  string `yaml:"image_repo"`
	TagPattern string `yaml:"tag_pattern"`
	// Node names the registered host, the same form deploy-root.yaml uses.
	// NodeID is the superseded way to name it, still read for the files
	// already committed.
	Node         string `yaml:"node"`
	NodeID       string `yaml:"node_id"`
	Space        string `yaml:"space"`
	RegistryUser string `yaml:"registry_user"`
}

// Handle reads image-watches.yaml at cmd.Ref and makes the stack's watches in
// Postgres match it exactly: entries missing in Postgres are created, entries
// whose fields changed are updated, and rows no longer named in the file are
// deleted — the file is the source of truth, so its silence about a watch
// means the watch should not exist.
//
// One malformed entry does not abort the others: a typo in one watch must not
// stop a push from applying every watch that is still correct.
func (h *Handler) Handle(ctx context.Context, cmd Command) error {
	raw, err := h.fetcher.FetchFileAtRef(ctx, cmd.Repo, ConfigFilePath, cmd.Ref, cmd.GitToken)
	if err != nil {
		if errors.Is(err, source.ErrFileNotFound) {
			return nil
		}
		return fmt.Errorf("fetch %s: %w", ConfigFilePath, err)
	}

	var file watchFile
	if err := yaml.Unmarshal([]byte(raw), &file); err != nil {
		slog.Warn("imageupdate: image-watches.yaml is not valid YAML, skipping reconciliation",
			"stack_id", cmd.StackID, "error", err)
		return nil
	}

	existing, err := h.repo.FindByStackID(ctx, cmd.StackID)
	if err != nil {
		return fmt.Errorf("list existing watches: %w", err)
	}
	// Only the rows THIS repo declared are this reconcile's to manage: several
	// repos feed one stack, and sweeping the whole stack made every push
	// delete the other repos' watches (and the API-created ones, whose
	// SourceRepo is empty). A legacy row matched by name is adopted so the
	// column backfills on the first push after the migration.
	byName := make(map[string]*domain.Watch, len(existing))
	for _, w := range existing {
		if w.SourceRepo == cmd.Repo || w.SourceRepo == "" {
			byName[w.Name] = w
		}
	}

	declared := make(map[string]bool, len(file.Watches))
	for _, entry := range file.Watches {
		if entry.Name == "" {
			slog.Warn("imageupdate: image-watches.yaml entry has no name, skipping", "stack_id", cmd.StackID)
			continue
		}
		declared[entry.Name] = true

		watch, err := h.resolve(ctx, cmd.StackID, entry)
		if err != nil {
			slog.Warn("imageupdate: could not reconcile watch from Git",
				"stack_id", cmd.StackID, "name", entry.Name, "error", err)
			continue
		}
		watch.SourceRepo = cmd.Repo

		if current, ok := byName[entry.Name]; ok {
			watch.ID = current.ID
			watch.Enabled = current.Enabled
			watch.CurrentTag = current.CurrentTag
			watch.Status = current.Status
			watch.CreatedAt = current.CreatedAt
			// The committed file can never carry the registry secret; it is
			// attached once via the API (manage_watch.SetRegistrySecret) and
			// must survive every subsequent push, or a private registry's
			// polling breaks again at the next reconcile.
			watch.RegistrySecret = current.RegistrySecret
			if err := h.repo.Update(ctx, watch); err != nil {
				slog.Warn("imageupdate: could not update watch from Git",
					"stack_id", cmd.StackID, "name", entry.Name, "error", err)
			}
			continue
		}
		if err := h.repo.Save(ctx, watch); err != nil {
			slog.Warn("imageupdate: could not create watch from Git",
				"stack_id", cmd.StackID, "name", entry.Name, "error", err)
		}
	}

	for name, w := range byName {
		if declared[name] {
			continue
		}
		if w.SourceRepo == "" {
			// Never sweep a row this repo did not declare: an empty SourceRepo
			// is an API/UI-created watch (or a pre-migration row another repo
			// owns) — deletion must come from its owner, not a bystander push.
			continue
		}
		if err := h.repo.Delete(ctx, w.ID); err != nil {
			slog.Warn("imageupdate: could not delete watch no longer declared in Git",
				"stack_id", cmd.StackID, "name", name, "error", err)
		}
	}

	return nil
}

func (h *Handler) resolve(ctx context.Context, stackID string, entry watchEntry) (*domain.Watch, error) {
	if entry.Node == "" && entry.NodeID == "" {
		return nil, fmt.Errorf("node is required")
	}
	if h.nodes == nil || h.decrypt == nil {
		return nil, fmt.Errorf("gateway is not configured on this deployment")
	}
	node, err := h.credentialFor(ctx, stackID, entry.Node, entry.NodeID)
	if err != nil {
		return nil, fmt.Errorf("resolve node %q: %w", entry.target(), err)
	}
	token, err := h.decrypt.Decrypt(node.SealedToken)
	if err != nil {
		return nil, fmt.Errorf("decrypt node token: %w", err)
	}

	if entry.Space == "" {
		return nil, fmt.Errorf("space is required: a node hosts several spaces")
	}
	watch, err := domain.NewWatch(stackID, entry.Name, entry.ImageRepo, entry.TagPattern,
		entry.Service, node.ServerURL, entry.Space, token, node.CAFingerprint)
	if err != nil {
		return nil, err
	}
	watch.RegistryUser = entry.RegistryUser
	return watch, nil
}

// target names whichever identity the entry declared, for messages.
func (e watchEntry) target() string {
	if e.Node != "" {
		return e.Node
	}
	return e.NodeID
}

// credentialFor hands back the credential for the node an entry names,
// preferring the operator-facing name over the superseded id.
func (h *Handler) credentialFor(ctx context.Context, stackID, nodeName, nodeID string) (NodeAddress, error) {
	if nodeName != "" {
		return h.nodes.Credential(ctx, stackID, nodeName)
	}
	return h.nodes.CredentialByID(ctx, stackID, nodeID)
}
