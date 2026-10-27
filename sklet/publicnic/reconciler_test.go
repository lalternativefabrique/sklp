package publicnic

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
)

type fakeHost struct {
	observed Observed
	leaseErr error
	applyErr map[Action]error
	applied  []Action
}

func (f *fakeHost) LinkIndex(string) (int, error) { return 3, nil }

func (f *fakeHost) Lease(int) (Lease, error) {
	if f.leaseErr != nil {
		return Lease{}, f.leaseErr
	}
	return Lease{Address: net.ParseIP("198.51.100.34").To4(), Router: net.ParseIP("198.51.100.1").To4()}, nil
}

func (f *fakeHost) Observe(Desired) (Observed, error) { return f.observed, nil }

func (f *fakeHost) Apply(_ Desired, r Repair) error {
	if err := f.applyErr[r.Action]; err != nil {
		return err
	}
	f.applied = append(f.applied, r.Action)
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReconcileHealthyTouchesNothing(t *testing.T) {
	h := &fakeHost{observed: Observed{DefaultRoute: &RouteState{Gateway: net.ParseIP("198.51.100.1"), LinkIndex: 3, OnLink: true}, RuleFrom: true, RuleMark: true}}
	r := NewReconciler(Config{}, h, quiet())
	if err := r.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(h.applied) != 0 {
		t.Fatalf("applied %v on a healthy host", h.applied)
	}
	st := r.Status()
	if st.LastRepair != nil || st.Repairs != 0 || st.LastCheck.IsZero() || st.Desired == nil {
		t.Fatalf("status %+v", st)
	}
}

func TestReconcileRecordsRepairs(t *testing.T) {
	h := &fakeHost{observed: Observed{RuleFrom: true, NftMissing: []string{"table"}}, applyErr: map[Action]error{ActionNft: errors.New("eperm")}}
	r := NewReconciler(Config{Source: "203.0.113.213"}, h, quiet())
	if err := r.Reconcile(); err == nil {
		t.Fatal("a failed repair must surface")
	}
	if len(h.applied) != 2 || h.applied[0] != ActionRoute || h.applied[1] != ActionRuleMark {
		t.Fatalf("applied %v", h.applied)
	}
	st := r.Status()
	if st.LastRepair == nil || len(st.LastRepair.Items) != 3 || len(st.LastRepair.Failed) != 1 || st.Repairs != 1 || st.LastError == "" {
		t.Fatalf("status %+v", st)
	}
	if st.Desired.Source.String() != "203.0.113.213" {
		t.Fatalf("source %s", st.Desired.Source)
	}
}

func TestReconcileWithoutLeaseRepairsNothing(t *testing.T) {
	h := &fakeHost{leaseErr: errors.New("no such file")}
	r := NewReconciler(Config{}, h, quiet())
	if err := r.Reconcile(); err == nil {
		t.Fatal("missing lease must surface")
	}
	if len(h.applied) != 0 || r.Status().LastError == "" {
		t.Fatalf("applied %v status %+v", h.applied, r.Status())
	}
}
