// Package domain holds the aggregate root and value objects of the image-update
// bounded context.
//
// A `Watch` binds one image repository to one service on one sklet node: when a
// newer tag appears in the registry, the watcher rolls that service onto it. It
// is the pull-based half of the deploy story — `sklp deploy` pushes a manifest
// from a workstation, a Watch reacts to a publish without anyone at a keyboard.
//
// The node stays the owner of what it runs: a roll names a service and a tag,
// never a manifest. Skalpai therefore needs no clone of the deployed repo and no
// copy of its deploy.yaml — see the /v1/image route in the sklp control API.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status enumerates a Watch's last known outcome. Reads as a state of the WATCH,
// not of a roll: `rolling` means a roll is in flight right now, which is what
// AbortOrphans resets when a replica dies mid-roll.
const (
	StatusIdle    = "idle"
	StatusRolling = "rolling"
	StatusFailed  = "failed"
	// StatusHeld means the last roll was rolled back by the node: the watch
	// keeps serving the previous tag and will not retry the held one.
	StatusHeld = "held"
)

// DefaultTagPattern matches the immutable YYYYMMDD-HHMM-<sha> tags every sklp
// publish emits. Lexical order equals chronological order for this shape, which
// is what lets the watcher pick the newest tag by sorting strings — no registry
// round-trip for a creation date that sklp does not write anyway.
const DefaultTagPattern = `^[0-9]{8}-[0-9]{4}-[0-9a-f]+$`

// DefaultSpace is the space a node serves when a Watch does not name one.
const DefaultSpace = "production"

// ErrValidation is the parent of every rejection NewWatch can produce, so the
// API maps a malformed watch to 400 without enumerating each cause — a new
// validation rule is a bad request by construction, not by remembering to add
// it to a switch.
var ErrValidation = errors.New("invalid image watch")

var (
	ErrEmptyName      = fmt.Errorf("%w: name is required", ErrValidation)
	ErrEmptyImageRepo = fmt.Errorf("%w: image repository is required", ErrValidation)
	ErrEmptyService   = fmt.Errorf("%w: service is required", ErrValidation)
	ErrEmptyNode      = fmt.Errorf("%w: node server URL is required", ErrValidation)
	ErrEmptyToken     = fmt.Errorf("%w: node token is required", ErrValidation)
	ErrNotFound       = errors.New("image watch not found")
)

// serviceNameRE mirrors the shape the sklp control API enforces on a service
// name. Rejecting it here means a Watch that could never roll cannot be saved.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Watch is the aggregate root. Every field is serialisable so the repository
// round-trips it without reaching for live handles.
//
// NodeToken and RegistrySecret hold the ENCRYPTED bytes: the repository seals
// them on write and opens them on read, so a plaintext credential never rests
// in a row nor crosses the API boundary in a DTO.
type Watch struct {
	ID      string
	StackID string
	Name    string

	// ImageRepo is the full repository reference WITHOUT a tag, e.g.
	// "rg.fr-par.scw.cloud/altfab/www".
	ImageRepo  string
	TagPattern string

	// Service is the service name on the node, and NodeServer/NodeSpace/
	// NodeToken/NodeCAFingerprint are how the dispatcher reaches it. The target
	// lives on the row rather than in an env var because one core serves many
	// stacks, each with its own node.
	Service           string
	NodeServer        string
	NodeSpace         string
	NodeToken         string
	NodeCAFingerprint string

	// RegistryUser / RegistrySecret authenticate the tag listing. Empty for a
	// public repository.
	RegistryUser   string
	RegistrySecret string
	// SourceRepo is the git repo (owner/name) whose image-watches.yaml
	// declared this watch; empty for watches created via the API/UI. The
	// git reconcile sweep only deletes rows it owns — several repos feed
	// one stack, and a stack-wide sweep deleted the other repos' watches.
	SourceRepo string

	Enabled    bool
	CurrentTag string
	Status     string
	LastError  string
	// HeldTag is a tag the node rolled back; the poller skips it until a newer
	// tag appears or an operator releases the hold. PinnedTag is a tag an
	// operator rolled by hand; the poller stays off the watch while it is set.
	HeldTag   string
	PinnedTag string

	LastCheckedAt *time.Time
	LastRolledAt  *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// NewWatch validates and builds a Watch in its initial state.
func NewWatch(stackID, name, imageRepo, tagPattern, service, nodeServer, nodeSpace, nodeToken, caFingerprint string) (*Watch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrEmptyName
	}
	imageRepo = strings.TrimSpace(imageRepo)
	if imageRepo == "" {
		return nil, ErrEmptyImageRepo
	}
	// A repository carrying a tag would make the watch meaningless: the tag is
	// what the watcher discovers.
	if tagged, _ := splitTag(imageRepo); tagged {
		return nil, fmt.Errorf("%w: image repository %q must not carry a tag", ErrValidation, imageRepo)
	}
	service = strings.TrimSpace(service)
	if service == "" {
		return nil, ErrEmptyService
	}
	if !serviceNameRE.MatchString(service) {
		return nil, fmt.Errorf("%w: service name %q must match %s", ErrValidation, service, serviceNameRE.String())
	}
	nodeServer = strings.TrimSpace(nodeServer)
	if nodeServer == "" {
		return nil, ErrEmptyNode
	}
	if !strings.HasPrefix(nodeServer, "https://") && !strings.HasPrefix(nodeServer, "http://") {
		return nil, fmt.Errorf("%w: node server %q must be an http(s) URL", ErrValidation, nodeServer)
	}
	if strings.TrimSpace(nodeToken) == "" {
		return nil, ErrEmptyToken
	}
	if tagPattern = strings.TrimSpace(tagPattern); tagPattern == "" {
		tagPattern = DefaultTagPattern
	}
	if _, err := regexp.Compile(tagPattern); err != nil {
		return nil, fmt.Errorf("%w: tag pattern: %s", ErrValidation, err)
	}
	if nodeSpace = strings.TrimSpace(nodeSpace); nodeSpace == "" {
		nodeSpace = DefaultSpace
	}

	now := time.Now().UTC()
	return &Watch{
		ID:                uuid.New().String(),
		StackID:           stackID,
		Name:              name,
		ImageRepo:         imageRepo,
		TagPattern:        tagPattern,
		Service:           service,
		NodeServer:        strings.TrimRight(nodeServer, "/"),
		NodeSpace:         nodeSpace,
		NodeToken:         nodeToken,
		NodeCAFingerprint: strings.TrimSpace(caFingerprint),
		Enabled:           true,
		Status:            StatusIdle,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// NewerTag returns the highest tag matching the watch's pattern that sorts
// strictly above CurrentTag, and whether one was found.
//
// Sorting is lexical, which is correct for the date-stamped shape sklp
// publishes and wrong for semver — a pattern admitting "v1.10.0" would rank it
// below "v1.9.0". That trade is deliberate: the alternative is reading each
// image's config .created field, which sklp writes as the zero time, making
// every fresh build look older than the one already deployed.
func (w *Watch) NewerTag(tags []string) (string, bool) {
	re, err := regexp.Compile(w.TagPattern)
	if err != nil {
		return "", false
	}
	best := ""
	for _, tag := range tags {
		if !re.MatchString(tag) || (w.HeldTag != "" && tag <= w.HeldTag) {
			continue
		}
		if tag > best {
			best = tag
		}
	}
	if best == "" || best <= w.CurrentTag {
		return "", false
	}
	return best, true
}

// TagMatches reports whether tag has the shape the watch rolls.
func (w *Watch) TagMatches(tag string) bool {
	re, err := regexp.Compile(w.TagPattern)
	return err == nil && re.MatchString(tag)
}

// ImageRef is the fully qualified reference to roll onto for a given tag.
func (w *Watch) ImageRef(tag string) string {
	return w.ImageRepo + ":" + tag
}

// splitTag reports whether ref carries a tag, ignoring a registry port (the
// colon in "host:5000/ns/img" belongs to the host, not to a tag).
func splitTag(ref string) (bool, string) {
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return true, ref[colon+1:]
	}
	return false, ""
}

// ClaimFor takes the watch for a roll onto toTag, reporting whether this
// caller won it. A watch already rolling, or already on that tag, is not
// claimed — that check is what keeps two replicas from dispatching the same
// roll, and it belongs here rather than in the SQL that used to hold it, so
// both the Postgres and the KV repository enforce one rule instead of two.
//
// The caller persists the watch when this returns true, under whatever
// concurrency control its store offers (a row lock, a compare-and-swap on the
// revision read before this call).
func (w *Watch) ClaimFor(toTag string, at time.Time) bool {
	if w.Status == StatusRolling || w.CurrentTag == toTag {
		return false
	}
	// A pin set for a DIFFERENT tag is stale once a roll to another one wins:
	// the operator's hold was on the tag being rolled away from.
	if w.PinnedTag != "" && w.PinnedTag != toTag {
		w.PinnedTag = ""
	}
	w.Status = StatusRolling
	w.UpdatedAt = at.UTC()
	return true
}

// Finish records the node's immediate answer to a roll request.
//
// RollResultApplied is deliberately not terminal: the node accepted the change
// but the rollout has not settled, so the watch stays rolling and the tag does
// not advance until Settle hears the outcome. Anything else that is not an
// error is the tag being live now.
func (w *Watch) Finish(roll *Roll, at time.Time) {
	utc := at.UTC()
	switch roll.Result {
	case RollResultApplied:
		return
	case RollResultUnchanged, RollResultRolled, RollResultLive:
		w.Status = StatusIdle
		w.CurrentTag = roll.ToTag
		w.LastError = ""
		w.LastRolledAt = &utc
	default:
		w.Status = StatusFailed
		w.LastError = roll.ErrorMessage
	}
	w.UpdatedAt = utc
}

// Settle records the rollout outcome the node reported after an applied roll.
//
// A rollback holds the tag rather than failing the watch: the image is bad,
// not the watch, so the poller must skip that tag and keep watching for a
// newer one. It also clears the pin, since an operator's manual roll onto a
// tag the node then rolled back should not keep the watch pointed at it.
func (w *Watch) Settle(roll *Roll, outcome RollOutcome) {
	at := outcome.FinishedAt.UTC()
	switch outcome.Result {
	case RollResultLive:
		w.Status = StatusIdle
		w.CurrentTag = roll.ToTag
		w.HeldTag = ""
		w.LastError = ""
		w.LastRolledAt = &at
	case RollResultRolledBack:
		w.Status = StatusHeld
		if roll.ToTag > w.HeldTag {
			w.HeldTag = roll.ToTag
		}
		w.PinnedTag = ""
		w.LastError = outcome.Reason
	default:
		w.Status = StatusFailed
		w.LastError = outcome.Reason
	}
	w.UpdatedAt = at
}
