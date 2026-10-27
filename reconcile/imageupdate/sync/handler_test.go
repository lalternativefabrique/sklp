package sync

import (
	"context"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/reconcile/source"
)

type stubFetcher struct {
	content string
	err     error
}

func (f stubFetcher) FetchFileAtRef(context.Context, string, string, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.content, nil
}

type stubNodes struct{ node *NodeAddress }

func (s stubNodes) Credential(_ context.Context, _, nodeName string) (NodeAddress, error) {
	if s.node == nil || nodeName != "flight" {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *s.node, nil
}

func (s stubNodes) CredentialByID(_ context.Context, _, nodeID string) (NodeAddress, error) {
	if s.node == nil || nodeID != "n1" {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *s.node, nil
}

var _ NodeDirectory = stubNodes{}

type staticDecrypter struct{ token string }

func (d staticDecrypter) Decrypt([]byte) (string, error) { return d.token, nil }

type stubRepo struct {
	byStack map[string][]*domain.Watch
	saved   []*domain.Watch
	updated []*domain.Watch
	deleted []string
}

func newStubRepo(existing ...*domain.Watch) *stubRepo {
	return &stubRepo{byStack: map[string][]*domain.Watch{"s1": existing}}
}

func (r *stubRepo) Save(_ context.Context, w *domain.Watch) error {
	r.saved = append(r.saved, w)
	return nil
}
func (r *stubRepo) Update(_ context.Context, w *domain.Watch) error {
	r.updated = append(r.updated, w)
	return nil
}
func (r *stubRepo) FindByID(context.Context, string) (*domain.Watch, error) { return nil, nil }
func (r *stubRepo) FindByStackID(_ context.Context, stackID string) ([]*domain.Watch, error) {
	return r.byStack[stackID], nil
}
func (r *stubRepo) Delete(_ context.Context, id string) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (r *stubRepo) FindDue(context.Context) ([]*domain.Watch, error)              { return nil, nil }
func (r *stubRepo) ClaimRoll(context.Context, string, string) (bool, error)       { return false, nil }
func (r *stubRepo) FinishRoll(context.Context, string, *domain.Roll) error        { return nil }
func (r *stubRepo) TouchChecked(context.Context, string, time.Time, string) error { return nil }
func (r *stubRepo) ListRolls(context.Context, string, int) ([]*domain.Roll, error) {
	return nil, nil
}
func (r *stubRepo) ResetOrphans(context.Context, string) (int, error) { return 0, nil }

func (r *stubRepo) SettleRoll(context.Context, string, *domain.Roll, domain.RollOutcome) error {
	return nil
}
func (r *stubRepo) ListPendingRolls(context.Context, time.Time) ([]*domain.Roll, error) {
	return nil, nil
}
func (r *stubRepo) SetPinnedTag(context.Context, string, string) error    { return nil }
func (r *stubRepo) AdoptCurrentTag(context.Context, string, string) error { return nil }
func (r *stubRepo) Release(context.Context, string) error                 { return nil }

var _ domain.Repository = (*stubRepo)(nil)

func testNode() *NodeAddress {
	return &NodeAddress{
		ServerURL:   "https://flight:6464",
		SealedToken: []byte("blob"), CAFingerprint: "sha256:deadbeef",
	}
}

func TestHandleCreatesWatchesDeclaredInGit(t *testing.T) {
	yaml := `
watches:
  - name: shop-core-test
    service: core
    image_repo: registry.example.com/shop/core-test
    node_id: n1
    space: production
`
	repo := newStubRepo()
	h := New(repo, stubFetcher{content: yaml}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("saved = %d watches, want 1", len(repo.saved))
	}
	if repo.saved[0].Name != "shop-core-test" || repo.saved[0].Service != "core" {
		t.Fatalf("saved watch = %+v, want name/service from YAML", repo.saved[0])
	}
}

func TestHandleDeletesOnlyOwnRepoWatches(t *testing.T) {
	// Owned by the pushing repo and no longer declared: swept.
	mine, _ := domain.NewWatch("s1", "stale-watch", "rg.example/svc", "", "web",
		"https://flight:6464", "production", "tok", "")
	mine.ID = "w1"
	mine.SourceRepo = "acme/widgets"
	// Owned by ANOTHER repo on the same stack: a bystander push must not touch it.
	other, _ := domain.NewWatch("s1", "other-app-web", "rg.example/other", "", "web",
		"https://flight:6464", "production", "tok", "")
	other.ID = "w2"
	other.SourceRepo = "acme/other"
	// API/UI-created (no source repo): never swept by a git push.
	manual, _ := domain.NewWatch("s1", "manual-watch", "rg.example/manual", "", "web",
		"https://flight:6464", "production", "tok", "")
	manual.ID = "w3"
	repo := newStubRepo(mine, other, manual)
	h := New(repo, stubFetcher{content: "watches: []"}, stubNodes{}, staticDecrypter{})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.deleted) != 1 || repo.deleted[0] != "w1" {
		t.Fatalf("deleted = %v, want [w1] only", repo.deleted)
	}
}

func TestHandleUpdatesExistingWatchPreservingRuntimeState(t *testing.T) {
	existing, _ := domain.NewWatch("s1", "shop-core-test", "rg.example/old", "", "web",
		"https://flight:6464", "production", "tok", "")
	existing.ID = "w1"
	existing.CurrentTag = "20260101-0000-abcdef0"
	existing.Enabled = false
	existing.RegistrySecret = "scw-poller-secret"
	repo := newStubRepo(existing)

	yaml := `
watches:
  - name: shop-core-test
    service: core
    image_repo: registry.example.com/shop/core-test
    node_id: n1
    space: production
`
	h := New(repo, stubFetcher{content: yaml}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("updated = %d watches, want 1", len(repo.updated))
	}
	got := repo.updated[0]
	if got.ID != "w1" {
		t.Fatalf("ID = %q, want the existing row's id preserved", got.ID)
	}
	if got.Service != "core" || got.ImageRepo != "registry.example.com/shop/core-test" {
		t.Fatalf("got = %+v, want fields refreshed from Git", got)
	}
	if got.CurrentTag != "20260101-0000-abcdef0" || got.Enabled != false {
		t.Fatalf("got = %+v, want runtime state (CurrentTag/Enabled) preserved", got)
	}
	// The committed file cannot carry the secret; wiping it on every push
	// would break private-registry polling at the first reconcile after
	// SetRegistrySecret.
	if got.RegistrySecret != "scw-poller-secret" {
		t.Fatalf("RegistrySecret = %q, want the out-of-band secret preserved", got.RegistrySecret)
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("deleted = %v, want none", repo.deleted)
	}
}

func TestHandleSkipsInvalidEntryButReconcilesTheRest(t *testing.T) {
	yaml := `
watches:
  - name: broken
    service: core
    image_repo: rg.fr-par.scw.cloud/broken
    node_id: missing-node
  - name: shop-core-test
    service: core
    image_repo: registry.example.com/shop/core-test
    node_id: n1
    space: production
`
	repo := newStubRepo()
	h := New(repo, stubFetcher{content: yaml}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.saved) != 1 || repo.saved[0].Name != "shop-core-test" {
		t.Fatalf("saved = %+v, want only the valid entry reconciled", repo.saved)
	}
}

func TestHandleNoOpsWhenFileIsAbsent(t *testing.T) {
	repo := newStubRepo()
	h := New(repo, stubFetcher{err: source.ErrFileNotFound}, stubNodes{}, staticDecrypter{})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.saved) != 0 || len(repo.updated) != 0 || len(repo.deleted) != 0 {
		t.Fatalf("expected no-op, got saved=%v updated=%v deleted=%v", repo.saved, repo.updated, repo.deleted)
	}
}

func TestHandleRequiresEachWatchToNameItsSpace(t *testing.T) {
	yaml := `
watches:
  - name: shop-core
    service: core
    image_repo: registry.example.com/shop/core
    node_id: n1
    space: shop-production
  - name: acme-core
    service: core
    image_repo: registry.example.com/acme/core
    node_id: n1
`
	repo := newStubRepo()
	h := New(repo, stubFetcher{content: yaml}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := map[string]string{}
	for _, w := range repo.saved {
		got[w.Name] = w.NodeSpace
	}
	if got["shop-core"] != "shop-production" {
		t.Fatalf("shop-core rolls into %q, want the declared shop-production", got["shop-core"])
	}
	if _, saved := got["acme-core"]; saved {
		t.Fatal("a watch naming no space must be skipped, not pointed at a guessed one")
	}
}

// countingNodes records which half of the directory a reconcile used, so a
// name-declared watch can be shown to resolve with a single credential lookup
// instead of the id translation it used to need.
type countingNodes struct {
	node  *NodeAddress
	byURL int
	byID  int
}

func (c *countingNodes) Credential(_ context.Context, _, nodeName string) (NodeAddress, error) {
	c.byURL++
	if c.node == nil || nodeName != "flight" {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *c.node, nil
}

func (c *countingNodes) CredentialByID(_ context.Context, _, nodeID string) (NodeAddress, error) {
	c.byID++
	if c.node == nil {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *c.node, nil
}

// The name a watch carries is the node, so resolving it asks only for the
// credential to reach it — never for an id to be translated first.
func TestHandleResolvesNodeNameWithoutIDLookup(t *testing.T) {
	yaml := `
watches:
  - name: shop-core
    service: core
    image_repo: rg.example/core
    node: flight
    space: production
`
	repo := newStubRepo()
	nodes := &countingNodes{node: testNode()}
	h := New(repo, stubFetcher{content: yaml}, nodes, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if nodes.byID != 0 {
		t.Errorf("CredentialByID calls = %d, want 0: a named node needs no id lookup", nodes.byID)
	}
	if len(repo.saved) != 1 || repo.saved[0].NodeServer != "https://flight:6464" {
		t.Fatalf("saved = %+v, want one watch pointing at the declared node", repo.saved)
	}
}

// The files already committed name a node_id and must keep reconciling.
func TestHandleStillResolvesLegacyNodeID(t *testing.T) {
	yaml := `
watches:
  - name: shop-core
    service: core
    image_repo: rg.example/core
    node_id: n1
    space: production
`
	nodes := &countingNodes{node: testNode()}
	repo := newStubRepo()
	h := New(repo, stubFetcher{content: yaml}, nodes, staticDecrypter{token: "s3cret"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if nodes.byID != 1 || nodes.byURL != 0 {
		t.Errorf("byID=%d byURL=%d, want the id path alone", nodes.byID, nodes.byURL)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("saved = %d, want 1", len(repo.saved))
	}
}

// A node this stack holds no credential for must create nothing: that lookup
// is what scopes a name to its tenant.
func TestHandleSkipsNodeWithNoCredential(t *testing.T) {
	yaml := `
watches:
  - name: shop-core
    service: core
    image_repo: rg.example/core
    node: someone-elses-node
    space: production
`
	repo := newStubRepo()
	h := New(repo, stubFetcher{content: yaml}, &countingNodes{node: testNode()}, staticDecrypter{token: "s"})

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.saved) != 0 {
		t.Fatalf("saved = %+v, want nothing for a node this stack cannot reach", repo.saved)
	}
}
