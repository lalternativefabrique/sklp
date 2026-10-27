package sklet

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/restore/drill"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/controller"
)

const restoreHistoryKeep = 60

// restoreHistory keeps each drill report the node has read back, one file per
// run, so the verdicts outlive the job logs and need nothing but the node.
type restoreHistory struct {
	dir string
}

// newRestoreHistory keeps the reports on the node's data disk, next to the
// volumes they vouch for; the state dir is only the fallback of a node with
// no data root.
func newRestoreHistory() *restoreHistory {
	root := xdg.DataRoot()
	if root == "" {
		root = xdg.StateRoot()
	}
	return &restoreHistory{dir: filepath.Join(root, "restores")}
}

func (h *restoreHistory) record(service string, rep drill.Report) (bool, error) {
	dir := filepath.Join(h.dir, rep.Space, service)
	name := filepath.Join(dir, fmt.Sprintf("%020d.json", rep.FinishedAt.UnixNano()))
	if _, err := os.Stat(name); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	raw, err := json.Marshal(rep)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(name+".tmp", raw, 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(name+".tmp", name); err != nil {
		return false, err
	}
	return true, h.prune(dir)
}

func (h *restoreHistory) prune(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) <= restoreHistoryKeep {
		return err
	}
	sort.Strings(files)
	for _, f := range files[:len(files)-restoreHistoryKeep] {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	return nil
}

type restoreTargetView struct {
	Name    string                  `json:"name"`
	Space   string                  `json:"space"`
	Service string                  `json:"service"`
	Engine  spacemanager.DataEngine `json:"engine"`
	Target  string                  `json:"target"`
	History []json.RawMessage       `json:"history"`
}

// list returns every target with its reports, newest first.
func (h *restoreHistory) list() ([]restoreTargetView, error) {
	spaces, err := os.ReadDir(h.dir)
	if os.IsNotExist(err) {
		return []restoreTargetView{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []restoreTargetView{}
	for _, sp := range spaces {
		services, err := os.ReadDir(filepath.Join(h.dir, sp.Name()))
		if err != nil {
			return nil, err
		}
		for _, svc := range services {
			files, err := filepath.Glob(filepath.Join(h.dir, sp.Name(), svc.Name(), "*.json"))
			if err != nil {
				return nil, err
			}
			sort.Sort(sort.Reverse(sort.StringSlice(files)))
			view := restoreTargetView{Name: sp.Name() + "/" + svc.Name(), Space: sp.Name(), Service: svc.Name(), History: []json.RawMessage{}}
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					return nil, err
				}
				view.History = append(view.History, raw)
			}
			if len(view.History) > 0 {
				var latest drill.Report
				if json.Unmarshal(view.History[0], &latest) == nil {
					view.Engine, view.Target = spacemanager.DataEngine(latest.Engine), latest.Target
				}
			}
			out = append(out, view)
		}
	}
	return out, nil
}

// jobLogLines reads the job's log files rather than the tailed ring: the ring
// polls, and the report is the last line a drill prints before it exits.
func jobLogLines(sup control.Supervisor, job string) []string {
	var lines []string
	for _, src := range sup.ServiceLogSources(job) {
		f, err := os.Open(src.Path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		f.Close()
	}
	if len(lines) == 0 {
		return sup.RecentLogLines(job)
	}
	return lines
}

// lastReport finds the newest report line in a drill job's log.
func lastReport(lines []string) (drill.Report, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		_, raw, ok := strings.Cut(lines[i], drill.ReportMarker)
		if !ok {
			continue
		}
		var rep drill.Report
		if json.Unmarshal([]byte(raw), &rep) == nil && !rep.FinishedAt.IsZero() {
			return rep, true
		}
	}
	return drill.Report{}, false
}

// restoreController keeps one derived restore space next to every production
// space holding a store the platform can read back, and archives what each
// drill reports. The restore spaces are never persisted: their jobs carry the
// production credentials, so they are rebuilt from the production manifest.
type restoreController struct {
	reg       *control.Registry
	load      func(space string) ([]spacemanager.Service, spacemanager.SpaceMeta, error)
	addSpace  func(ctx context.Context, opts controller.SpaceOptions) error
	options   func(space string) (controller.SpaceOptions, error)
	skletPath string
	history   *restoreHistory
	logf      func(format string, a ...any)

	mu      sync.Mutex
	pending map[string]bool
}

func (c *restoreController) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *restoreController) tick(ctx context.Context) {
	names := c.reg.Names()
	sort.Strings(names)
	for _, name := range names {
		if strings.HasSuffix(name, "-restore") {
			c.collect(name)
			continue
		}
		services, meta, err := c.load(name)
		if err != nil {
			continue
		}
		jobs, _ := spacemanager.PlanRestores(name, services, c.skletPath)
		if len(jobs) == 0 {
			continue
		}
		c.converge(ctx, spacemanager.RestoreSpaceName(name), jobs, meta)
	}
}

func (c *restoreController) converge(ctx context.Context, space string, jobs []spacemanager.Service, prod spacemanager.SpaceMeta) {
	sup, ok := c.reg.Supervisor(space)
	if !ok {
		c.mu.Lock()
		if c.pending[space] {
			c.mu.Unlock()
			return
		}
		c.pending[space] = true
		c.mu.Unlock()
		opts, err := c.options(space)
		if err != nil {
			c.logf("sklet agent: restore space %s: %v\n", space, err)
			return
		}
		opts.Services = jobs
		opts.Meta.Registries = prod.Registries
		go func() {
			defer func() {
				c.mu.Lock()
				delete(c.pending, space)
				c.mu.Unlock()
			}()
			if err := c.addSpace(ctx, opts); err != nil {
				c.logf("sklet agent: restore space %s: %v\n", space, err)
			}
		}()
		return
	}
	want := map[string]bool{}
	for _, job := range jobs {
		want[job.Name] = true
		if _, known := sup.DesiredService(job.Name); !known {
			sup.AddService(job)
		} else if sup.ServiceDiffers(job) {
			sup.Reconfigure(job)
		}
	}
	for _, name := range sup.DesiredServiceNames() {
		if !want[name] {
			sup.RemoveService(name)
		}
	}
}

func (c *restoreController) collect(space string) {
	sup, ok := c.reg.Supervisor(space)
	if !ok {
		return
	}
	for _, job := range sup.DesiredServiceNames() {
		rep, ok := lastReport(jobLogLines(sup, job))
		if !ok {
			continue
		}
		service := strings.TrimPrefix(job, "drill-")
		if fresh, err := c.history.record(service, rep); err != nil {
			c.logf("sklet agent: restore %s/%s: keep report: %v\n", rep.Space, service, err)
		} else if fresh {
			c.logf("sklet agent: restore %s/%s: %s\n", rep.Space, service, rep.Summary())
		}
	}
}

func startRestoreController(ctx context.Context, reg *control.Registry, sc *controller.SpaceController, gatewayMode string, shippers *shipperSet, out io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(out, "sklet agent: restore drills disabled: %v\n", err)
		return
	}
	history := newRestoreHistory()
	control.SetRestores(control.RestoreSourceFunc(func(context.Context) (json.RawMessage, error) {
		targets, err := history.list()
		if err != nil {
			return nil, err
		}
		return json.Marshal(targets)
	}))
	c := &restoreController{
		reg:      reg,
		load:     control.LoadManifest,
		addSpace: sc.AddSpace,
		options: func(space string) (controller.SpaceOptions, error) {
			return agentSpaceOptions(space, gatewayMode, reg, shippers)
		},
		skletPath: AgentBinaryPath(home),
		history:   history,
		logf:      func(format string, a ...any) { fmt.Fprintf(out, format, a...) },
		pending:   map[string]bool{},
	}
	go c.run(ctx, time.Minute)
}
