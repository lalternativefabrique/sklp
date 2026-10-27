package drill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type fakeEngine struct {
	clk        *clock
	backup     Backup
	locateErr  error
	restoreErr error
	startErr   error
	inspectErr error
	inv        Inventory
	checks     []Check
	stopped    bool
}

func (f *fakeEngine) Kind() string   { return "fake" }
func (f *fakeEngine) Target() string { return "t" }
func (f *fakeEngine) Locate(context.Context) (Backup, error) {
	f.clk.advance(time.Second)
	return f.backup, f.locateErr
}
func (f *fakeEngine) Restore(context.Context, Backup) error {
	f.clk.advance(40 * time.Second)
	return f.restoreErr
}
func (f *fakeEngine) Start(context.Context) (Instance, error) {
	f.clk.advance(20 * time.Second)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return f, nil
}
func (f *fakeEngine) Inspect(context.Context) (Inventory, error) {
	f.clk.advance(5 * time.Second)
	return f.inv, f.inspectErr
}
func (f *fakeEngine) Verify(context.Context, Inventory) []Check { return f.checks }
func (f *fakeEngine) Stop(context.Context) error {
	f.stopped = true
	return nil
}

func newFake() (*fakeEngine, *clock) {
	clk := &clock{t: time.Date(2026, 9, 26, 5, 0, 0, 0, time.UTC)}
	return &fakeEngine{
		clk:    clk,
		backup: Backup{Label: "20260926-030002D", TakenAt: time.Date(2026, 9, 26, 3, 0, 0, 0, time.UTC)},
		inv: Inventory{
			RecoveryPoint: time.Date(2026, 9, 26, 4, 30, 0, 0, time.UTC),
			Bytes:         2048,
			Collections:   []Collection{{Group: "app", Name: "public.users", Count: 1234, Bytes: 2048}},
		},
		checks: []Check{{Name: "integrity", OK: true}},
	}, clk
}

func TestRunMeasuresRTOAndRPO(t *testing.T) {
	eng, clk := newFake()
	rep := Run(context.Background(), eng, Options{Space: "app-production", MaxRPO: 26 * time.Hour, Now: clk.now})

	if rep.Status != StatusOK {
		t.Fatalf("status = %s, failures %v", rep.Status, rep.Failures())
	}
	if got := time.Duration(rep.RTO); got != 60*time.Second {
		t.Fatalf("rto = %s, want restore+start = 1m0s", got)
	}
	if got := time.Duration(rep.RPO); got != 31*time.Minute+6*time.Second {
		t.Fatalf("rpo = %s, want the distance from the recovery point to the end of the drill", got)
	}
	if !eng.stopped {
		t.Fatal("the throwaway instance was left running")
	}
	names := []string{}
	for _, p := range rep.Phases {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "locate,restore,start,inspect,verify,stop" {
		t.Fatalf("phases = %v", names)
	}
}

func TestRunFailsAStaleRecoveryPoint(t *testing.T) {
	eng, clk := newFake()
	eng.inv.RecoveryPoint = clk.t.Add(-72 * time.Hour)
	rep := Run(context.Background(), eng, Options{MaxRPO: 26 * time.Hour, Now: clk.now})
	if rep.Status != StatusFailed {
		t.Fatal("a restore three days behind passed a 26h budget")
	}
	if f := rep.Failures(); len(f) != 1 || f[0].Name != "recovery-point" {
		t.Fatalf("failures = %v", f)
	}
}

func TestRunFallsBackToTheBackupTimeWithoutARecoveryPoint(t *testing.T) {
	eng, clk := newFake()
	eng.inv.RecoveryPoint = time.Time{}
	rep := Run(context.Background(), eng, Options{Now: clk.now})
	if !rep.Inventory.RecoveryPoint.Equal(eng.backup.TakenAt) {
		t.Fatalf("recovery point = %s, want the backup time", rep.Inventory.RecoveryPoint)
	}
}

func TestRunStopsAtTheFirstBrokenPhase(t *testing.T) {
	eng, clk := newFake()
	eng.restoreErr = errors.New("repo unreachable")
	rep := Run(context.Background(), eng, Options{Now: clk.now})
	if rep.Status != StatusFailed {
		t.Fatal("a failed restore reported ok")
	}
	if last := rep.Phases[len(rep.Phases)-1]; last.Name != "restore" || last.Error != "repo unreachable" {
		t.Fatalf("last phase = %+v", last)
	}
	if !strings.Contains(rep.Summary(), "restore: repo unreachable") {
		t.Fatalf("summary hides the cause: %s", rep.Summary())
	}
}

func TestRunStopsTheInstanceWhenInspectFails(t *testing.T) {
	eng, clk := newFake()
	eng.inspectErr = errors.New("connection refused")
	rep := Run(context.Background(), eng, Options{Now: clk.now})
	if !eng.stopped || rep.Status != StatusFailed {
		t.Fatalf("stopped=%v status=%s", eng.stopped, rep.Status)
	}
}

func TestRunFailsOnAFailedEngineCheck(t *testing.T) {
	eng, clk := newFake()
	eng.checks = []Check{{Name: "integrity", OK: false, Detail: "index corrupt"}}
	if rep := Run(context.Background(), eng, Options{Now: clk.now}); rep.Status != StatusFailed {
		t.Fatal("a corrupt index passed")
	}
}

func TestReportJSONCarriesDurationsAsSeconds(t *testing.T) {
	eng, clk := newFake()
	raw, err := json.Marshal(Run(context.Background(), eng, Options{Now: clk.now}))
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		RTO float64 `json:"rto"`
	}
	if err := json.Unmarshal(raw, &back); err != nil || back.RTO != 60 {
		t.Fatalf("rto = %v (%v) in %s", back.RTO, err, raw)
	}
}

func TestRenderListsEveryCollection(t *testing.T) {
	eng, clk := newFake()
	var b strings.Builder
	Run(context.Background(), eng, Options{Space: "s", Now: clk.now}).Render(&b)
	out := b.String()
	for _, want := range []string{"public.users", "1 234", "2.0 KiB", "[ok  ] integrity"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render misses %q:\n%s", want, out)
		}
	}
}

func TestRunPassesAnEmptyStoreWhateverItsAge(t *testing.T) {
	eng, clk := newFake()
	eng.inv = Inventory{RecoveryPoint: clk.t.Add(-14 * 24 * time.Hour)}
	rep := Run(context.Background(), eng, Options{MaxRPO: 26 * time.Hour, Now: clk.now})
	if rep.Status != StatusOK {
		t.Fatalf("an empty store failed on its age: %v", rep.Failures())
	}
}
