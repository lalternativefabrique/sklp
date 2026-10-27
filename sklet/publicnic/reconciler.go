package publicnic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Host is the kernel side: links, routes, rules and the nft table.
type Host interface {
	LinkIndex(name string) (int, error)
	Lease(linkIndex int) (Lease, error)
	Observe(d Desired) (Observed, error)
	Apply(d Desired, r Repair) error
}

type RepairRecord struct {
	At     time.Time `json:"at"`
	Items  []Repair  `json:"items"`
	Failed []string  `json:"failed,omitempty"`
}

type Status struct {
	Config     Config        `json:"config"`
	Desired    *Desired      `json:"desired,omitempty"`
	Observed   *Observed     `json:"observed,omitempty"`
	LastCheck  time.Time     `json:"last_check"`
	LastRepair *RepairRecord `json:"last_repair,omitempty"`
	Repairs    int           `json:"repairs"`
	LastError  string        `json:"last_error,omitempty"`
}

type Reconciler struct {
	cfg  Config
	host Host
	log  *slog.Logger
	now  func() time.Time

	mu     sync.Mutex
	status Status
}

func NewReconciler(cfg Config, host Host, log *slog.Logger) *Reconciler {
	cfg = cfg.WithDefaults()
	return &Reconciler{cfg: cfg, host: host, log: log, now: time.Now, status: Status{Config: cfg}}
}

func (r *Reconciler) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Reconciler) Reconcile() error {
	desired, observed, repairs, err := r.check()
	r.mu.Lock()
	r.status.LastCheck = r.now()
	r.status.Desired, r.status.Observed = desired, observed
	r.mu.Unlock()
	if err != nil {
		return r.fail(err)
	}
	if len(repairs) == 0 {
		return r.fail(nil)
	}
	rec := RepairRecord{At: r.now(), Items: repairs}
	var errs []error
	for _, rp := range repairs {
		if aerr := r.host.Apply(*desired, rp); aerr != nil {
			rec.Failed = append(rec.Failed, fmt.Sprintf("%s: %v", rp.Action, aerr))
			errs = append(errs, fmt.Errorf("%s: %w", rp.Action, aerr))
			r.log.Error("public nic repair failed", "interface", desired.Interface, "action", string(rp.Action), "missing", rp.Detail, "err", aerr)
			continue
		}
		r.log.Warn("public nic repaired", "interface", desired.Interface, "action", string(rp.Action), "missing", rp.Detail)
	}
	r.mu.Lock()
	r.status.LastRepair = &rec
	r.status.Repairs++
	r.mu.Unlock()
	return r.fail(errors.Join(errs...))
}

func (r *Reconciler) check() (*Desired, *Observed, []Repair, error) {
	idx, err := r.host.LinkIndex(r.cfg.Interface)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("link %s: %w", r.cfg.Interface, err)
	}
	lease, err := r.host.Lease(idx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("lease for %s: %w", r.cfg.Interface, err)
	}
	d, err := Resolve(r.cfg, idx, lease)
	if err != nil {
		return nil, nil, nil, err
	}
	o, err := r.host.Observe(d)
	if err != nil {
		return &d, nil, nil, fmt.Errorf("observe %s: %w", r.cfg.Interface, err)
	}
	return &d, &o, Plan(d, o), nil
}

func (r *Reconciler) fail(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.status.LastError = ""
		return nil
	}
	r.status.LastError = err.Error()
	return err
}

// Run reconciles every interval and whenever events fires; a nil events
// channel leaves the ticker alone.
func (r *Reconciler) Run(ctx context.Context, every time.Duration, events <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Reconcile(); err != nil {
			r.log.Error("public nic reconcile", "interface", r.cfg.Interface, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-events:
		}
	}
}
