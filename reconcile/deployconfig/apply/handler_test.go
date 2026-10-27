package apply

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
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
	if s.node == nil || nodeName != "node-b" {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *s.node, nil
}

func (s stubNodes) CredentialByID(_ context.Context, _, nodeID string) (NodeAddress, error) {
	if s.node == nil || nodeID != "node-1" {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *s.node, nil
}

var _ NodeDirectory = stubNodes{}

type staticDecrypter struct{ token string }

func (d staticDecrypter) Decrypt([]byte) (string, error) { return d.token, nil }

type stubApplier struct {
	result   domain.ApplyResult
	err      error
	calls    int
	lastTgt  domain.NodeTarget
	lastMani *domain.Manifest
}

func (a *stubApplier) Apply(_ context.Context, target domain.NodeTarget, m *domain.Manifest) (domain.ApplyResult, error) {
	a.calls++
	a.lastTgt = target
	a.lastMani = m
	if a.err != nil {
		return domain.ApplyResult{}, a.err
	}
	return a.result, nil
}

// stubState defaults to AllReady:true (zero-value bool would silently make
// every existing "successful apply" test assert Ready=false, which is not
// what those tests are about) — a test that cares about ready=false or a
// state-check error sets fields explicitly.
type stubState struct {
	state domain.NodeState
	err   error
	calls int
}

func newStubState() *stubState {
	return &stubState{state: domain.NodeState{AllReady: true}}
}

func (s *stubState) State(context.Context, domain.NodeTarget) (domain.NodeState, error) {
	s.calls++
	if s.err != nil {
		return domain.NodeState{}, s.err
	}
	return s.state, nil
}

// stubSecrets records the (projectID, userID) it was called with, so a test
// can assert Handle threads cmd.ProjectID/cmd.UserID through to the
// resolver correctly.
type stubSecrets struct {
	secrets       map[string]string
	err           error
	lastProjectID string
	lastUserID    string
	lastRefs      []domain.SecretRef
}

func (s *stubSecrets) Resolve(_ context.Context, req domain.SecretRequest) (map[string]string, error) {
	s.lastProjectID = req.ProjectID
	s.lastUserID = req.UserID
	s.lastRefs = req.Refs
	if s.err != nil {
		return nil, s.err
	}
	return s.secrets, nil
}

type stubRepo struct {
	byStackSpace map[string]*domain.Deployment
	saved        []*domain.Deployment
	updated      []*domain.Deployment
}

func newStubRepo(existing ...*domain.Deployment) *stubRepo {
	r := &stubRepo{byStackSpace: map[string]*domain.Deployment{}}
	for _, d := range existing {
		r.byStackSpace[d.StackID+"/"+d.Space] = d
	}
	return r
}

func (r *stubRepo) Save(_ context.Context, d *domain.Deployment) error {
	r.saved = append(r.saved, d)
	return nil
}
func (r *stubRepo) Update(_ context.Context, d *domain.Deployment) error {
	r.updated = append(r.updated, d)
	return nil
}
func (r *stubRepo) FindBySpace(_ context.Context, stackID, space string) (*domain.Deployment, error) {
	return r.byStackSpace[stackID+"/"+space], nil
}
func (r *stubRepo) FindByStackID(_ context.Context, stackID string) ([]*domain.Deployment, error) {
	var out []*domain.Deployment
	for _, d := range r.byStackSpace {
		if d.StackID == stackID {
			out = append(out, d)
		}
	}
	return out, nil
}
func (r *stubRepo) FindApplied(context.Context) ([]*domain.Deployment, error) {
	var out []*domain.Deployment
	for _, d := range r.byStackSpace {
		if !d.Ready && d.LastError == "" && d.LastAppliedAt != nil {
			out = append(out, d)
		}
	}
	return out, nil
}
func (r *stubRepo) Delete(context.Context, string) error { return nil }

var _ domain.Repository = (*stubRepo)(nil)

const manifestPath = ".sklp/prod/deploy.yaml"

const validRoot = `
kind: deploy-root
node_id: node-1
deployments:
  - space: production
    path: ` + manifestPath + `
`

// rootFetcher serves the deploy-root and one manifest at manifestPath.
func rootFetcher(manifest string) pathFetcher {
	return pathFetcher{files: map[string]string{domain.RootPath: validRoot, manifestPath: manifest}}
}

func testNode() *NodeAddress {
	return &NodeAddress{
		ServerURL:   "https://node-b:6464",
		SealedToken: []byte("blob"), CAFingerprint: "sha256:deadbeef",
	}
}

const validYAML = `
services:
  api:
    image: registry.example.com/acme/api:20260828-0900-abc
    port: 8080
`

func TestHandleAppliesNewDeployment(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("Apply calls = %d, want 1", applier.calls)
	}
	if applier.lastTgt.ServerURL != "https://node-b:6464" || applier.lastTgt.Token != "s3cret" {
		t.Fatalf("unexpected target: %+v", applier.lastTgt)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("saved = %d, want 1 (new deployment)", len(repo.saved))
	}
	if repo.saved[0].LastAppliedHash == "" || repo.saved[0].LastError != "" {
		t.Fatalf("saved deployment = %+v, want a recorded hash and no error", repo.saved[0])
	}
}

// An unchanged manifest still reaches the node: the apply is idempotent and
// cheap there, and it is the only path that lets a node record what Git
// declares (an empty commit after a swap, a lost manifest after a rebuild).
// Skipping on the hash left a node's bookkeeping frozen until the next real
// content change.
func TestHandleReappliesAnUnchangedManifest(t *testing.T) {
	m, err := domain.ParseManifest(context.Background(), []byte(validYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	existing, err := domain.NewDeployment("s1", manifestPath, "", "node-1", "production")
	if err != nil {
		t.Fatalf("NewDeployment: %v", err)
	}
	existing.RecordApplied(m.Hash(), time.Now())
	repo := newStubRepo(existing)
	applier := &stubApplier{}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("Apply calls = %d, want 1 (the node decides what is unchanged)", applier.calls)
	}
	if len(repo.updated) != 1 || repo.updated[0].LastAppliedHash != m.Hash() {
		t.Fatalf("updated = %d, want the existing row re-recorded with the same hash", len(repo.updated))
	}
}

func TestHandleReappliesOnHashChange(t *testing.T) {
	stale, err := domain.NewDeployment("s1", manifestPath, "", "node-1", "production")
	if err != nil {
		t.Fatalf("NewDeployment: %v", err)
	}
	stale.RecordApplied("stale-hash", time.Now())
	repo := newStubRepo(stale)
	applier := &stubApplier{result: domain.ApplyResult{Result: "configured"}}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("Apply calls = %d, want 1 (hash changed)", applier.calls)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("updated = %d, want 1 (existing row)", len(repo.updated))
	}
	if repo.updated[0].LastAppliedHash == "stale-hash" {
		t.Fatal("LastAppliedHash was not refreshed after a successful re-apply")
	}
}

func TestHandleRecordsApplyFailureWithoutAborting(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{err: errors.New("node refused the apply: status 409")}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v, want nil (an apply failure is logged, not returned)", err)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("saved = %d, want 1 (the failure is still recorded)", len(repo.saved))
	}
	if repo.saved[0].LastError == "" {
		t.Fatal("LastError was not recorded on an apply failure")
	}
	if repo.saved[0].LastAppliedHash != "" {
		t.Fatalf("LastAppliedHash = %q, want empty (the apply never succeeded)", repo.saved[0].LastAppliedHash)
	}
}

// TestHandleRecordsReadyOnSuccessfulHealthCheck: an apply followed by an
// AllReady=true state check marks the deployment Ready — the load-bearing
// happy path for issue 206ec5bd's step 2.
func TestHandleRecordsReadyOnSuccessfulHealthCheck(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	state := newStubState() // AllReady: true by construction
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, state, nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if state.calls != 1 {
		t.Fatalf("State calls = %d, want 1", state.calls)
	}
	if len(repo.saved) != 1 || !repo.saved[0].Ready {
		t.Fatalf("saved deployment = %+v, want Ready=true", repo.saved[0])
	}
	if repo.saved[0].LastCheckedAt == nil {
		t.Fatal("LastCheckedAt was not recorded")
	}
}

// TestHandleRecordsNotReadyWithoutTreatingItAsApplyFailure: AllReady=false
// means the apply succeeded (the manifest WAS pushed) but the service has
// not converged yet — this must not be confused with an apply failure
// (LastError stays empty, LastAppliedHash stays set).
func TestHandleRecordsNotReadyWithoutTreatingItAsApplyFailure(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	state := &stubState{state: domain.NodeState{AllReady: false}}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, state, nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := repo.saved[0]
	if got.Ready {
		t.Fatal("Ready = true, want false (AllReady was false)")
	}
	if got.LastError != "" {
		t.Fatalf("LastError = %q, want empty — not-yet-ready is not an apply failure", got.LastError)
	}
	if got.LastAppliedHash == "" {
		t.Fatal("LastAppliedHash was cleared, want it kept — the apply itself succeeded")
	}
}

// TestHandleRecordsHealthCheckTransportError: a health check that cannot
// even REACH the node (as opposed to reaching it and getting AllReady=false)
// is a distinct outcome — RecordHealth's doc calls this out — and DOES set
// LastError, since it is itself a failure, just not of the apply.
func TestHandleRecordsHealthCheckTransportError(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	state := &stubState{err: errors.New("reach node: connection refused")}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, state, nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := repo.saved[0]
	if got.Ready {
		t.Fatal("Ready = true, want false (the check itself failed)")
	}
	if got.LastError == "" {
		t.Fatal("LastError was not recorded on a health-check transport failure")
	}
	if got.LastAppliedHash == "" {
		t.Fatal("LastAppliedHash was cleared, want it kept — the apply itself succeeded")
	}
}

// TestHandleSkipsHealthCheckOnApplyFailure: an apply that never succeeded
// has nothing to health-check — asserts checkHealth is not reached at all
// on that path (stubState.calls stays 0), not just that Ready ends up
// false.
func TestHandleSkipsHealthCheckOnApplyFailure(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{err: errors.New("node refused the apply: status 409")}
	state := newStubState()
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, state, nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if state.calls != 0 {
		t.Fatalf("State calls = %d, want 0 (an apply failure has nothing to check)", state.calls)
	}
}

func TestHandleNoOpsWhenFileIsAbsent(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{}
	h := New(repo, stubFetcher{err: source.ErrFileNotFound}, stubNodes{}, staticDecrypter{}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 0 || len(repo.saved) != 0 || len(repo.updated) != 0 {
		t.Fatalf("expected no-op, got apply calls=%d saved=%v updated=%v", applier.calls, repo.saved, repo.updated)
	}
}

func TestHandleSkipsWhenNodeNotFound(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{}
	h := New(repo, rootFetcher(validYAML), stubNodes{}, staticDecrypter{}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v, want nil (an unresolvable node is logged, not fatal)", err)
	}
	if applier.calls != 0 || len(repo.saved) != 0 {
		t.Fatalf("expected no-op, got apply calls=%d saved=%v", applier.calls, repo.saved)
	}
}

// TestHandleSkipsSecretReferencingManifestWithoutResolver: no
// domain.SecretResolver wired (h.secrets == nil) — a ${VAR} reference is
// logged and skipped, not fatal. See
// TestHandleResolvesSecretReferenceWithResolver for the resolver-present
// path (issue 206ec5bd's step 4).
func TestHandleSkipsSecretReferencingManifestWithoutResolver(t *testing.T) {
	yaml := `
services:
  api:
    image: img:tag
    env:
      S3_SECRET_KEY: ${S3_SECRET_KEY}
`
	repo := newStubRepo()
	applier := &stubApplier{}
	h := New(repo, rootFetcher(yaml), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v, want nil (a ${VAR} reference is logged, not fatal)", err)
	}
	if applier.calls != 0 || len(repo.saved) != 0 {
		t.Fatalf("expected no-op, got apply calls=%d saved=%v", applier.calls, repo.saved)
	}
}

// TestHandleResolvesSecretReferenceWithResolver: a ${VAR} reference resolves
// through h.secrets and reaches the applier with the plaintext value — the
// end-to-end happy path for issue 206ec5bd's step 4, threading
// cmd.ProjectID/cmd.UserID into the resolver call.
func TestHandleResolvesSecretReferenceWithResolver(t *testing.T) {
	yaml := `
services:
  api:
    image: img:tag
    env:
      S3_SECRET_KEY: ${S3_SECRET_KEY}
`
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	secrets := &stubSecrets{secrets: map[string]string{"S3_SECRET_KEY": "resolved-value"}}
	h := New(repo, rootFetcher(yaml), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), secrets)

	if err := h.Handle(context.Background(), Command{StackID: "s1", ProjectID: "proj-1", UserID: "user-1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("Apply calls = %d, want 1", applier.calls)
	}
	if secrets.lastProjectID != "proj-1" || secrets.lastUserID != "user-1" {
		t.Fatalf("resolver called with project=%q user=%q, want proj-1/user-1", secrets.lastProjectID, secrets.lastUserID)
	}
	if len(applier.lastMani.Services[0].Env) != 1 || applier.lastMani.Services[0].Env[0] != "S3_SECRET_KEY=resolved-value" {
		t.Fatalf("applied env = %v, want the resolved value", applier.lastMani.Services[0].Env)
	}
}

// TestHandleSkipsWhenSecretNotFound: a ${VAR} naming a secret the resolver
// did not return is logged and skipped, same tolerance as any other
// ParseManifest error.
func TestHandleSkipsWhenSecretNotFound(t *testing.T) {
	yaml := `
services:
  api:
    image: img:tag
    env:
      MISSING: ${MISSING}
`
	repo := newStubRepo()
	applier := &stubApplier{}
	secrets := &stubSecrets{secrets: map[string]string{}}
	h := New(repo, rootFetcher(yaml), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), secrets)

	if err := h.Handle(context.Background(), Command{StackID: "s1", ProjectID: "proj-1", UserID: "user-1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v, want nil", err)
	}
	if applier.calls != 0 || len(repo.saved) != 0 {
		t.Fatalf("expected no-op, got apply calls=%d saved=%v", applier.calls, repo.saved)
	}
}

// pathFetcher serves a different file per path, unlike stubFetcher which
// answers everything with one content.
type pathFetcher struct{ files map[string]string }

func (f pathFetcher) FetchFileAtRef(_ context.Context, _, path, _, _ string) (string, error) {
	if c, ok := f.files[path]; ok {
		return c, nil
	}
	return "", source.ErrFileNotFound
}

// The point of deploy-root: one repo, several spaces, all applied on one push.
func TestHandleAppliesEveryDeploymentTheRootDeclares(t *testing.T) {
	root := `
kind: deploy-root
node_id: node-1
deployments:
  - space: demo-production
    path: .sklp/prod/deploy.yaml
  - space: piper-production
    path: .sklp/prod/piper.yaml
`
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(repo, pathFetcher{files: map[string]string{
		domain.RootPath:          root,
		".sklp/prod/deploy.yaml": validYAML,
		".sklp/prod/piper.yaml":  validYAML,
	}}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 2 {
		t.Fatalf("Apply calls = %d, want 2 (one per declared space)", applier.calls)
	}
	// One row per manifest path, else the second space would overwrite the
	// first's recorded state and each push would look like a change.
	spaces := map[string]bool{}
	for _, d := range repo.saved {
		spaces[d.Space] = true
	}
	if !spaces["demo-production"] || !spaces["piper-production"] {
		t.Fatalf("saved spaces = %v, want both declared spaces", spaces)
	}
}

// The root is the only source of destination: a manifest with no
// deploy-root beside it is never applied, even at the historical path.
func TestHandleSkipsWithoutDeployRoot(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(repo, pathFetcher{files: map[string]string{
		manifestPath: validYAML,
	}}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 0 || len(repo.saved) != 0 {
		t.Fatalf("expected no-op, got apply calls=%d saved=%v", applier.calls, repo.saved)
	}
}

// The destination comes from the root entry, never from the manifest.
func TestHandleTakesDestinationFromTheRoot(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.lastMani.NodeID != "node-1" || applier.lastTgt.Space != "production" {
		t.Fatalf("destination = %q/%q, want node-1/production from the root", applier.lastMani.NodeID, applier.lastTgt.Space)
	}
	if len(repo.saved) != 1 || repo.saved[0].Name != manifestPath {
		t.Fatalf("saved = %+v, want one row named after the manifest path", repo.saved)
	}
}

// A manifest the root points at but that does not exist must not stop the
// other deployments the same push declares.
func TestHandleSkipsAMissingManifestAndKeepsGoing(t *testing.T) {
	root := `
kind: deploy-root
node_id: node-1
deployments:
  - space: gone-production
    path: .sklp/prod/gone.yaml
  - space: piper-production
    path: .sklp/prod/piper.yaml
`
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(newStubRepo(), pathFetcher{files: map[string]string{
		domain.RootPath:         root,
		".sklp/prod/piper.yaml": validYAML,
	}}, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 1 {
		t.Fatalf("Apply calls = %d, want 1 (the surviving deployment)", applier.calls)
	}
}

// countingNodes records which half of the directory a reconcile used, so a
// name-declared root can be shown to resolve with a single credential lookup
// instead of the id translation it used to need.
type countingNodes struct {
	node     *NodeAddress
	byURL    int
	byID     int
	lastAddr string
}

func (c *countingNodes) Credential(_ context.Context, _, nodeName string) (NodeAddress, error) {
	c.byURL++
	c.lastAddr = nodeName
	if c.node == nil {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *c.node, nil
}

func (c *countingNodes) CredentialByID(_ context.Context, _, nodeID string) (NodeAddress, error) {
	c.byID++
	c.lastAddr = nodeID
	if c.node == nil {
		return NodeAddress{}, ErrNodeNotFound
	}
	return *c.node, nil
}

const namedRoot = `
kind: deploy-root
node: node-b
deployments:
  - space: production
    path: ` + manifestPath + `
`

// The name a deploy-root carries is the node, so the reconcile asks only for
// the credential to reach it — never for an id to be translated first.
func TestHandleResolvesNodeNameWithoutIDLookup(t *testing.T) {
	nodes := &countingNodes{node: testNode()}
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	fetcher := pathFetcher{files: map[string]string{domain.RootPath: namedRoot, manifestPath: validYAML}}
	h := New(newStubRepo(), fetcher, nodes, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if nodes.byID != 0 {
		t.Errorf("CredentialByID calls = %d, want 0: a named node needs no id lookup", nodes.byID)
	}
	if nodes.byURL != 1 || nodes.lastAddr != "node-b" {
		t.Errorf("Credential calls = %d for %q, want 1 for the declared node", nodes.byURL, nodes.lastAddr)
	}
	if applier.calls != 1 || applier.lastTgt.Token != "s3cret" {
		t.Errorf("unexpected apply: calls=%d target=%+v", applier.calls, applier.lastTgt)
	}
}

// A root still naming node_id keeps reconciling through the registry.
func TestHandleStillResolvesLegacyNodeID(t *testing.T) {
	nodes := &countingNodes{node: testNode()}
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(newStubRepo(), rootFetcher(validYAML), nodes, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if nodes.byID != 1 || nodes.byURL != 0 {
		t.Errorf("byID=%d byURL=%d, want the id path alone", nodes.byID, nodes.byURL)
	}
	if applier.calls != 1 {
		t.Errorf("Apply calls = %d, want 1", applier.calls)
	}
}

// A node the stack holds no credential for must not deploy: the credential
// lookup is what scopes a name to its tenant.
func TestHandleSkipsNodeWithNoCredential(t *testing.T) {
	nodes := &countingNodes{node: nil}
	applier := &stubApplier{}
	fetcher := pathFetcher{files: map[string]string{domain.RootPath: namedRoot, manifestPath: validYAML}}
	h := New(newStubRepo(), fetcher, nodes, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if applier.calls != 0 {
		t.Fatalf("Apply calls = %d, want 0 for a node this stack cannot reach", applier.calls)
	}
}

// Two repos of one stack conventionally declare the same manifest path
// (.sklp/prod/deploy.yaml). Keyed by that path they shared one row, so every
// push rewrote whichever app had won the previous one — acme, shop and
// shop took turns owning a single deployment in production. The space is
// what is actually deployed, and it stays distinct.
func TestHandleKeepsOneDeploymentPerSpaceAcrossRepos(t *testing.T) {
	shop, err := domain.NewDeployment("s1", manifestPath, "node-b", "", "shop-production")
	if err != nil {
		t.Fatalf("NewDeployment: %v", err)
	}
	shop.RecordApplied("shop-hash", time.Now())
	repo := newStubRepo(shop)

	// A second repo pushes the SAME path, for a different space.
	root := `
kind: deploy-root
node: node-b
deployments:
  - space: acme-production
    path: ` + manifestPath + `
`
	fetcher := pathFetcher{files: map[string]string{domain.RootPath: root, manifestPath: validYAML}}
	applier := &stubApplier{result: domain.ApplyResult{Result: "created"}}
	h := New(repo, fetcher, stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/acme", Ref: "abc"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(repo.saved) != 1 {
		t.Fatalf("saved = %d, want 1: a new space is its own deployment", len(repo.saved))
	}
	if repo.saved[0].Space != "acme-production" {
		t.Errorf("saved space = %q, want acme-production", repo.saved[0].Space)
	}
	if len(repo.updated) != 0 {
		t.Errorf("updated = %d, want 0: the other repo's space must not be rewritten", len(repo.updated))
	}
	if shop.Space != "shop-production" || shop.LastAppliedHash != "shop-hash" {
		t.Errorf("the existing deployment was overwritten: %+v", shop)
	}
}

// A reconcile that dies before the apply used to leave the row exactly as the
// last successful push left it: ready, no error, a stale hash. The UI then
// showed green while nothing had been applied for hours — which is how a
// broken reconcile went unnoticed until someone read the pod logs.
func TestHandleRecordsAFailureThatHappensBeforeTheApply(t *testing.T) {
	existing := appliedDeployment(t, "s1", "production")
	repo := newStubRepo(existing)
	// A manifest referencing a ${VAR} no store can resolve: the reconcile stops
	// at parse time, before any apply.
	manifest := `
services:
  api:
    image: registry.example.com/acme/api:20260828-0900-abc
    port: 8080
    env:
      TOKEN: ${MISSING}
`
	secrets := &stubSecrets{err: errors.New("secret MISSING not found")}
	applier := &stubApplier{}
	h := New(repo, rootFetcher(manifest), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), secrets)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if applier.calls != 0 {
		t.Fatalf("Apply calls = %d, want 0 — the manifest never parsed", applier.calls)
	}
	if len(repo.updated) != 1 {
		t.Fatalf("updated = %d, want the existing row marked", len(repo.updated))
	}
	got := repo.updated[0]
	if got.LastError == "" {
		t.Error("LastError is empty; the UI has nothing to show and reads as healthy")
	}
	if got.Ready {
		t.Error("Ready is still true after a reconcile that applied nothing")
	}
}

// The node being unreachable is the same class of failure: nothing was
// applied, so the row must say so rather than keep its last good state.
func TestHandleRecordsAnUnresolvableNode(t *testing.T) {
	existing := appliedDeployment(t, "s1", "production")
	repo := newStubRepo(existing)
	applier := &stubApplier{}
	h := New(repo, rootFetcher(validYAML), stubNodes{}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(repo.updated) != 1 {
		t.Fatalf("updated = %d, want the existing row marked", len(repo.updated))
	}
	if repo.updated[0].LastError == "" {
		t.Error("an unreachable node left no error on the deployment")
	}
}

// A deployment Skalpai has never applied has no row to mark, and inventing one
// would put a deployment in the UI that does not exist. The warning in the log
// stays the only trace, which is correct for something never deployed.
func TestHandleDoesNotInventARowForANeverAppliedDeployment(t *testing.T) {
	repo := newStubRepo()
	applier := &stubApplier{}
	h := New(repo, rootFetcher(validYAML), stubNodes{}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(repo.saved) != 0 || len(repo.updated) != 0 {
		t.Fatalf("saved=%d updated=%d, want neither for a deployment that never existed",
			len(repo.saved), len(repo.updated))
	}
}

// A reconcile that succeeds must clear a previous failure, or the row stays
// red forever once anything has gone wrong.
func TestHandleClearsThePreviousErrorOnASuccessfulApply(t *testing.T) {
	existing := appliedDeployment(t, "s1", "production")
	existing.RecordFailure("secret MISSING not found")
	repo := newStubRepo(existing)
	applier := &stubApplier{result: domain.ApplyResult{Result: "configured"}}
	h := New(repo, rootFetcher(validYAML), stubNodes{node: testNode()}, staticDecrypter{token: "s3cret"}, applier, newStubState(), nil)

	if err := h.Handle(context.Background(), Command{StackID: "s1", Repo: "acme/widgets", Ref: "abc123"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if len(repo.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(repo.updated))
	}
	if repo.updated[0].LastError != "" {
		t.Errorf("LastError = %q, want cleared by a successful apply", repo.updated[0].LastError)
	}
}

// appliedDeployment is a row in the state a successful push leaves behind:
// applied, ready, no error.
func appliedDeployment(t *testing.T, stackID, space string) *domain.Deployment {
	t.Helper()
	d, err := domain.NewDeployment(stackID, manifestPath, "node-b", "", space)
	if err != nil {
		t.Fatalf("NewDeployment: %v", err)
	}
	d.RecordApplied("previous-hash", time.Now())
	d.RecordHealth(true, "", time.Now())
	return d
}
