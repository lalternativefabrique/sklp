package sklet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/controller"
)

type fakeRestoreSup struct {
	control.Supervisor
	desired map[string]spacemanager.Service
	logs    map[string]string
	dir     string
}

func (f *fakeRestoreSup) DesiredServiceNames() []string {
	var out []string
	for n := range f.desired {
		out = append(out, n)
	}
	return out
}
func (f *fakeRestoreSup) DesiredService(n string) (spacemanager.Service, bool) {
	s, ok := f.desired[n]
	return s, ok
}
func (f *fakeRestoreSup) ServiceDiffers(s spacemanager.Service) bool {
	return f.desired[s.Name].Image != s.Image
}
func (f *fakeRestoreSup) AddService(s spacemanager.Service)  { f.desired[s.Name] = s }
func (f *fakeRestoreSup) Reconfigure(s spacemanager.Service) { f.desired[s.Name] = s }
func (f *fakeRestoreSup) RemoveService(n string)             { delete(f.desired, n) }
func (f *fakeRestoreSup) RecentLogLines(string) []string     { return nil }
func (f *fakeRestoreSup) ServiceLogSources(n string) []spacemanager.LogSource {
	body, ok := f.logs[n]
	if !ok {
		return nil
	}
	p := filepath.Join(f.dir, n+".log")
	os.WriteFile(p, []byte(body), 0o600)
	return []spacemanager.LogSource{{Label: n, Path: p}}
}

func prodServices(image string) []spacemanager.Service {
	return []spacemanager.Service{
		{Name: "postgres", Image: image, Volumes: []string{"pgdata:/var/lib/postgresql/data"},
			Env: []string{"PGBACKREST_STANZA=shop", "PGBACKREST_REPO1_S3_BUCKET=shop-backups", "PGBACKREST_REPO1_S3_KEY=AK", "PGBACKREST_REPO1_S3_KEY_SECRET=SK"}},
		{Name: "web", Image: "shop/web:1"},
	}
}

type harness struct {
	reg      *control.Registry
	c        *restoreController
	prod     []spacemanager.Service
	added    []controller.SpaceOptions
	mu       sync.Mutex
	restore  *fakeRestoreSup
	stateDir string
}

func newHarness(t *testing.T) *harness {
	h := &harness{reg: control.NewRegistry(), prod: prodServices("postgres:17-alpine"), stateDir: t.TempDir()}
	h.reg.Register("shop-production", &fakeRestoreSup{desired: map[string]spacemanager.Service{}}, func() {})
	h.c = &restoreController{
		reg: h.reg,
		load: func(space string) ([]spacemanager.Service, spacemanager.SpaceMeta, error) {
			return h.prod, spacemanager.SpaceMeta{Name: space, Registries: map[string]spacemanager.RegistryAuthMeta{"rg.example": {Username: "nologin"}}}, nil
		},
		addSpace: func(_ context.Context, opts controller.SpaceOptions) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.added = append(h.added, opts)
			return nil
		},
		options: func(space string) (controller.SpaceOptions, error) {
			return controller.SpaceOptions{Meta: spacemanager.SpaceMeta{Name: space}}, nil
		},
		skletPath: "/home/user/.local/bin/sklet",
		history:   &restoreHistory{dir: h.stateDir},
		logf:      func(string, ...any) {},
		pending:   map[string]bool{},
	}
	return h
}

func (h *harness) waitAdded(t *testing.T) controller.SpaceOptions {
	t.Helper()
	for i := 0; i < 100; i++ {
		h.mu.Lock()
		n := len(h.added)
		h.mu.Unlock()
		if n > 0 {
			return h.added[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no restore space was brought up")
	return controller.SpaceOptions{}
}

func TestRestoreControllerBringsUpADerivedSpace(t *testing.T) {
	h := newHarness(t)
	h.c.tick(context.Background())
	opts := h.waitAdded(t)
	if opts.Meta.Name != "shop-production-restore" || len(opts.Services) != 1 || opts.Services[0].Name != "drill-postgres" {
		t.Fatalf("restore space = %+v", opts)
	}
	if _, ok := opts.Meta.Registries["rg.example"]; !ok {
		t.Fatal("the restore space cannot pull the production image without its registry credentials")
	}
}

func TestRestoreControllerFollowsTheProductionImage(t *testing.T) {
	h := newHarness(t)
	sup := &fakeRestoreSup{desired: map[string]spacemanager.Service{}}
	h.reg.Register("shop-production-restore", sup, func() {})
	h.c.tick(context.Background())
	if sup.desired["drill-postgres"].Image != "postgres:17-alpine" {
		t.Fatalf("desired = %+v", sup.desired)
	}
	h.prod = prodServices("postgres:17.6-alpine")
	h.c.tick(context.Background())
	if sup.desired["drill-postgres"].Image != "postgres:17.6-alpine" {
		t.Fatal("the drill kept restoring with the image production left")
	}
	h.prod = []spacemanager.Service{{Name: "web", Image: "shop/web:1"}}
	sup.desired["drill-gone"] = spacemanager.Service{Name: "drill-gone"}
	h.c.tick(context.Background())
	if _, ok := sup.desired["drill-gone"]; !ok {
		t.Fatal("a space with nothing to drill any more must be left alone, not emptied job by job")
	}
}

func TestRestoreControllerArchivesEachReportOnce(t *testing.T) {
	h := newHarness(t)
	rep := drill.Report{Space: "shop-production", Engine: "postgres", Target: "shop", Status: drill.StatusOK,
		FinishedAt: time.Date(2026, 9, 27, 4, 12, 0, 0, time.UTC)}
	raw, _ := json.Marshal(rep)
	sup := &fakeRestoreSup{
		desired: map[string]spacemanager.Service{"drill-postgres": {Name: "drill-postgres"}},
		logs:    map[string]string{"drill-postgres": "[drill-postgres] restore drill · …\n[drill-postgres] " + drill.ReportMarker + string(raw) + "\n[drill-postgres] restore-drill: ok\n"},
		dir:     t.TempDir(),
	}
	h.reg.Register("shop-production-restore", sup, func() {})
	h.c.tick(context.Background())
	h.c.tick(context.Background())

	targets, err := h.c.history.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Name != "shop-production/postgres" || targets[0].Engine != "postgres" || len(targets[0].History) != 1 {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestRestoreHistoryKeepsTheNewestRuns(t *testing.T) {
	h := &restoreHistory{dir: t.TempDir()}
	base := time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC)
	for i := 0; i < restoreHistoryKeep+5; i++ {
		if _, err := h.record("postgres", drill.Report{Space: "s", FinishedAt: base.Add(time.Duration(i) * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	targets, _ := h.list()
	if len(targets[0].History) != restoreHistoryKeep {
		t.Fatalf("kept %d runs, want %d", len(targets[0].History), restoreHistoryKeep)
	}
	var newest drill.Report
	json.Unmarshal(targets[0].History[0], &newest)
	if !strings.HasPrefix(newest.FinishedAt.Format(time.DateOnly), "2026-03") {
		t.Fatalf("newest first expected, got %s", newest.FinishedAt)
	}
}
