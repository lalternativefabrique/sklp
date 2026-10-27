package drill

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Status string

const (
	StatusOK     Status = "ok"
	StatusFailed Status = "failed"
)

type Backup struct {
	Label   string    `json:"label"`
	Type    string    `json:"type,omitempty"`
	TakenAt time.Time `json:"taken_at"`
	Bytes   int64     `json:"bytes,omitempty"`
}

// Collection is one unit of restored data: a table for Postgres, a stream
// for NATS, a keyspace for Redis. Count is exact, never an estimate.
type Collection struct {
	Group  string     `json:"group"`
	Name   string     `json:"name"`
	Count  int64      `json:"count"`
	Bytes  int64      `json:"bytes"`
	Newest *time.Time `json:"newest,omitempty"`
}

type Inventory struct {
	RecoveryPoint time.Time    `json:"recovery_point"`
	Position      string       `json:"position,omitempty"`
	Bytes         int64        `json:"bytes"`
	Collections   []Collection `json:"collections"`
}

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type Instance interface {
	Inspect(ctx context.Context) (Inventory, error)
	Verify(ctx context.Context, inv Inventory) []Check
	Stop(ctx context.Context) error
}

// Engine restores one kind of data store into scratch space and brings up a
// throwaway instance over it. Adding a store means adding an Engine; the
// lifecycle, timing and reporting stay in Run.
type Engine interface {
	Kind() string
	Target() string
	Locate(ctx context.Context) (Backup, error)
	Restore(ctx context.Context, b Backup) error
	Start(ctx context.Context) (Instance, error)
}

type Phase struct {
	Name     string   `json:"name"`
	Duration Duration `json:"duration"`
	Error    string   `json:"error,omitempty"`
}

type Report struct {
	Space      string    `json:"space"`
	Engine     string    `json:"engine"`
	Target     string    `json:"target"`
	Status     Status    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Duration   Duration  `json:"duration"`
	RTO        Duration  `json:"rto"`
	RPO        Duration  `json:"rpo"`
	Backup     Backup    `json:"backup"`
	Inventory  Inventory `json:"inventory"`
	Phases     []Phase   `json:"phases"`
	Checks     []Check   `json:"checks"`
}

func (r Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

type Options struct {
	Space  string
	MaxRPO time.Duration
	Now    func() time.Time
}

func Run(ctx context.Context, eng Engine, opts Options) Report {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rep := Report{Space: opts.Space, Engine: eng.Kind(), Target: eng.Target(), StartedAt: opts.Now().UTC()}
	run(ctx, eng, opts, &rep)
	rep.FinishedAt = opts.Now().UTC()
	rep.Duration = Duration(rep.FinishedAt.Sub(rep.StartedAt))
	rep.Status = StatusOK
	if len(rep.Failures()) > 0 {
		rep.Status = StatusFailed
	}
	return rep
}

func run(ctx context.Context, eng Engine, opts Options, rep *Report) {
	now := opts.Now
	phase := func(name string, fn func() error) bool {
		start := now()
		err := fn()
		p := Phase{Name: name, Duration: Duration(now().Sub(start))}
		if err != nil {
			p.Error = err.Error()
			rep.Checks = append(rep.Checks, Check{Name: name, OK: false, Detail: err.Error()})
		}
		rep.Phases = append(rep.Phases, p)
		return err == nil
	}

	if !phase("locate", func() error {
		b, err := eng.Locate(ctx)
		rep.Backup = b
		return err
	}) {
		return
	}
	restoreStart := now()
	if !phase("restore", func() error { return eng.Restore(ctx, rep.Backup) }) {
		return
	}
	var inst Instance
	if !phase("start", func() error {
		var err error
		inst, err = eng.Start(ctx)
		return err
	}) {
		return
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		phase("stop", func() error { return inst.Stop(stopCtx) })
	}()
	rep.RTO = Duration(now().Sub(restoreStart))

	if !phase("inspect", func() error {
		inv, err := inst.Inspect(ctx)
		rep.Inventory = inv
		return err
	}) {
		return
	}
	phase("verify", func() error {
		rep.Checks = append(rep.Checks, inst.Verify(ctx, rep.Inventory)...)
		return nil
	})
	rep.Checks = append(rep.Checks, freshness(rep, opts.MaxRPO, now()))
}

func freshness(rep *Report, maxRPO time.Duration, now time.Time) Check {
	point := rep.Inventory.RecoveryPoint
	if point.IsZero() {
		point = rep.Backup.TakenAt
	}
	if point.IsZero() {
		return Check{Name: "recovery-point", OK: false, Detail: "the restore exposes no recovery point"}
	}
	rep.Inventory.RecoveryPoint = point
	rep.RPO = Duration(now.Sub(point))
	if len(rep.Inventory.Collections) == 0 {
		return Check{Name: "recovery-point", OK: true, Detail: fmt.Sprintf("empty store, nothing written since %s", point.Format(time.RFC3339))}
	}
	if maxRPO > 0 && now.Sub(point) > maxRPO {
		return Check{Name: "recovery-point", OK: false,
			Detail: fmt.Sprintf("restored data stops at %s, %s ago — over the %s budget", point.Format(time.RFC3339), rep.RPO, Duration(maxRPO))}
	}
	return Check{Name: "recovery-point", OK: true, Detail: fmt.Sprintf("data up to %s (%s ago)", point.Format(time.RFC3339), rep.RPO)}
}

var ErrNoBackup = errors.New("no backup found")

// ReportMarker prefixes the one log line carrying the JSON report.
const ReportMarker = "restore-drill report: "
