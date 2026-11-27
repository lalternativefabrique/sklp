package sklpmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/remoteapply"
	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/wire"
)

const (
	maxContexts     = 100
	maxSpaces       = 200
	maxServices     = 100
	maxInstances    = 20
	maxHistory      = 5
	maxTailLines    = 20
	maxChanges      = 100
	defaultLogLines = 100
	maxLogLines     = 1000
	maxLineBytes    = 2000
)

func (n *node) tools() []toolDef {
	return append([]toolDef{
		define(&mcp.Tool{Name: "deploy_spaces", Description: "List the spaces the node runs, with their status and container counts."}, read, n.listSpaces),
		define(&mcp.Tool{Name: "deploy_get", Description: "Show one space as its supervisor sees it: each service and job, its image, " +
			"instances (state, restarts, uptime), recent rollouts and job runs."}, read, n.getSpace),
		define(&mcp.Tool{Name: "deploy_manifest", Description: "Show the manifest the space was declared with: services, jobs, images, ports, routes, " +
			"env keys. Values of ${VAR} references and secret-looking literals are never returned."}, read, n.getManifest),
		define(&mcp.Tool{Name: "deploy_logs", Description: "Return the last lines a service or job wrote to stdout/stderr (no follow)."}, read, n.serviceLogs),
		define(&mcp.Tool{Name: "deploy_datastores", Description: "List the services keeping data in named volumes and how well each is protected " +
			"(none, opaque, backed, disposable), with the last backup outcome."}, read, n.listDataStores),
		define(&mcp.Tool{Name: "deploy_apply", Description: "Deploy a stack from a local repository to a space of the node, like `sklp deploy apply <stack> -space <space>`: " +
			"sends .sklp/<stack>/deploy.yaml and the ${VAR} values it references (from env_files, stack config and the environment). " +
			"Creates the space, or reconciles it toward the file.",
			Annotations: &mcp.ToolAnnotations{IdempotentHint: true}}, destructive, n.applyStack),
		define(&mcp.Tool{Name: "deploy_exec", Description: "Run a command in a live instance of a service on the node, without a terminal or stdin " +
			"(sklp deploy exec <service> -space <space> -- <cmd>). Returns its exit code and the last lines of its output."}, destructive, n.execService),
		define(&mcp.Tool{Name: "deploy_restart", Description: "Bounce every live instance of a service in a space."}, destructive, n.restartService),
		define(&mcp.Tool{Name: "deploy_run_job", Description: "Re-run a oneshot job of a space that reached a terminal state. Fails if a run is in flight."}, destructive, n.runJob),
		define(&mcp.Tool{Name: "deploy_delete", Description: "Stop a space and remove its manifest so it does not come back. " +
			"With volumes=true its named volumes, and the data in them, are destroyed too, irreversibly."}, destructive, n.deleteSpace),
	}, append(n.followTools(), n.watchTools()...)...)
}

type noInput struct{}

type spaceInput struct {
	Space string `json:"space" jsonschema:"name of the space on the node (see deploy_spaces)"`
}

func requireSpace(space string) error {
	if space == "" {
		return errors.New("space is required (see deploy_spaces)")
	}
	return nil
}

type contextRow struct {
	Name    string `json:"name"`
	Server  string `json:"server,omitempty"`
	Space   string `json:"space,omitempty"`
	Bound   bool   `json:"bound,omitempty"`
	Current bool   `json:"current,omitempty"`
}

type contextsOut struct {
	Bound     string       `json:"bound,omitempty"`
	Contexts  []contextRow `json:"contexts"`
	Truncated bool         `json:"truncated,omitempty"`
}

func listContexts(n *node) func(context.Context, noInput, *redactor) (contextsOut, error) {
	return func(context.Context, noInput, *redactor) (contextsOut, error) {
		cfg, _, err := config.Load()
		if err != nil {
			return contextsOut{}, err
		}
		names := make([]string, 0, len(cfg.Contexts))
		for name := range cfg.Contexts {
			names = append(names, name)
		}
		sort.Strings(names)
		out := contextsOut{Contexts: []contextRow{}}
		if n != nil {
			out.Bound = n.context
		}
		for _, name := range names {
			if len(out.Contexts) == maxContexts {
				out.Truncated = true
				break
			}
			c := cfg.Contexts[name]
			out.Contexts = append(out.Contexts, contextRow{
				Name: name, Server: c.Server, Space: c.Space,
				Bound: out.Bound == name, Current: name == cfg.CurrentContext,
			})
		}
		return out, nil
	}
}

type spaceRow struct {
	Name       string   `json:"name"`
	Status     string   `json:"status,omitempty"`
	Supervised bool     `json:"supervised"`
	Containers int      `json:"containers"`
	Running    int      `json:"running"`
	Volumes    []string `json:"volumes,omitempty"`
}

type spacesOut struct {
	Node      string     `json:"node,omitempty"`
	Spaces    []spaceRow `json:"spaces"`
	Truncated bool       `json:"truncated,omitempty"`
}

func (n *node) listSpaces(ctx context.Context, _ noInput, _ *redactor) (spacesOut, error) {
	c, err := n.spaceClient("")
	if err != nil {
		return spacesOut{}, err
	}
	res, err := c.Spaces(ctx)
	if err != nil {
		return spacesOut{}, err
	}
	var infos []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Supervised bool   `json:"supervised"`
		Containers []struct {
			Status string `json:"status"`
		} `json:"containers"`
		Volumes []string `json:"volumes"`
	}
	if err := json.Unmarshal(res.Spaces, &infos); err != nil {
		return spacesOut{}, fmt.Errorf("decode node inventory: %w", err)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	out := spacesOut{Node: res.Node, Spaces: []spaceRow{}}
	for _, in := range infos {
		if len(out.Spaces) == maxSpaces {
			out.Truncated = true
			break
		}
		row := spaceRow{Name: in.Name, Status: in.Status, Supervised: in.Supervised, Containers: len(in.Containers), Volumes: head(in.Volumes, maxInstances)}
		for _, ct := range in.Containers {
			if ct.Status == "running" {
				row.Running++
			}
		}
		out.Spaces = append(out.Spaces, row)
	}
	return out, nil
}

type instanceOut struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	Port      int        `json:"port,omitempty"`
	Restarts  int        `json:"restarts,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	RestartAt *time.Time `json:"restart_at,omitempty"`
	BackoffMS int64      `json:"backoff_ms,omitempty"`
	JobFails  int        `json:"job_fails,omitempty"`
	Held      bool       `json:"held,omitempty"`
}

type serviceOut struct {
	Name           string                 `json:"name"`
	State          string                 `json:"state"`
	Image          string                 `json:"image,omitempty"`
	Job            bool                   `json:"job,omitempty"`
	RejectedReason string                 `json:"rejected_reason,omitempty"`
	Instances      []instanceOut          `json:"instances"`
	Rollouts       []control.RolloutState `json:"rollouts,omitempty"`
	Runs           []control.JobRunState  `json:"runs,omitempty"`
}

type spaceOut struct {
	Space     string       `json:"space"`
	AllReady  bool         `json:"all_ready"`
	Services  []serviceOut `json:"services"`
	Truncated bool         `json:"truncated,omitempty"`
}

func (n *node) getSpace(ctx context.Context, in spaceInput, _ *redactor) (spaceOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return spaceOut{}, err
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return spaceOut{}, err
	}
	st, err := c.State(ctx)
	if err != nil {
		return spaceOut{}, err
	}
	// An empty space is vacuously all-ready on the node; to a reader that
	// says "healthy", the opposite of a space with nothing in it.
	out := spaceOut{Space: in.Space, AllReady: st.AllReady && len(st.Services) > 0, Services: []serviceOut{}}
	for _, svc := range st.Services {
		if len(out.Services) == maxServices {
			out.Truncated = true
			break
		}
		so := serviceOut{Name: svc.Name, State: svc.State, Image: svc.Image, Job: svc.Oneshot, RejectedReason: svc.RejectedReason, Instances: []instanceOut{}}
		for _, inst := range head(svc.Instances, maxInstances) {
			so.Instances = append(so.Instances, instanceOut{
				ID: inst.ID, State: inst.State, Port: inst.Port, Restarts: inst.Restarts,
				StartedAt: inst.StartedAt, RestartAt: inst.RestartAt, BackoffMS: inst.BackoffMS,
				JobFails: inst.JobFails, Held: inst.Held,
			})
		}
		for _, ro := range tail(svc.Rollouts, maxHistory) {
			ro.LogTail = tail(ro.LogTail, maxTailLines)
			so.Rollouts = append(so.Rollouts, ro)
		}
		for _, run := range head(svc.Runs, maxHistory) {
			run.LogTail = tail(run.LogTail, maxTailLines)
			so.Runs = append(so.Runs, run)
		}
		out.Services = append(out.Services, so)
	}
	return out, nil
}

func (n *node) getManifest(ctx context.Context, in spaceInput, _ *redactor) (control.ManifestResponse, error) {
	if err := requireSpace(in.Space); err != nil {
		return control.ManifestResponse{}, err
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return control.ManifestResponse{}, err
	}
	m, err := c.Manifest(ctx)
	if err != nil {
		return control.ManifestResponse{}, err
	}
	m.Services = withheldEnv(head(m.Services, maxServices))
	m.Jobs = withheldEnv(head(m.Jobs, maxServices))
	if m.Services == nil {
		m.Services = []control.ManifestService{}
	}
	return m, nil
}

func withheldEnv(svcs []control.ManifestService) []control.ManifestService {
	out := make([]control.ManifestService, 0, len(svcs))
	for _, s := range svcs {
		env := make([]control.ManifestEnv, 0, len(s.Env))
		for _, e := range s.Env {
			if e.Source == control.EnvSourceSensitive {
				e.Value = ""
			}
			env = append(env, e)
		}
		s.Env = env
		out = append(out, s)
	}
	return out
}

type logsInput struct {
	Space   string `json:"space" jsonschema:"name of the space on the node"`
	Service string `json:"service" jsonschema:"service or job name (see deploy_get)"`
	Lines   int    `json:"lines,omitempty" jsonschema:"how many of the last lines to return (default 100, at most 1000)"`
}

type logsOut struct {
	Space     string   `json:"space"`
	Service   string   `json:"service"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (n *node) serviceLogs(ctx context.Context, in logsInput, r *redactor) (logsOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return logsOut{}, err
	}
	if in.Service == "" {
		return logsOut{}, errors.New("service is required (see deploy_get)")
	}
	lines := in.Lines
	if lines <= 0 {
		lines = defaultLogLines
	}
	lines = min(lines, maxLogLines)
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return logsOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ring := &lineRing{max: lines}
	if err := c.Logs(ctx, in.Service, false, ring); err != nil {
		return logsOut{}, err
	}
	got, dropped := ring.snapshot()
	for i, l := range got {
		got[i] = r.str(l)
	}
	return logsOut{Space: in.Space, Service: in.Service, Lines: nonNil(got), Truncated: dropped}, nil
}

type applyInput struct {
	Repo  string `json:"repo" jsonschema:"path of the local repository holding .sklp/<stack>/deploy.yaml (relative paths resolve against the server's working directory)"`
	Stack string `json:"stack" jsonschema:"stack to deploy: the <stack> of .sklp/<stack>/deploy.yaml"`
	Space string `json:"space" jsonschema:"space of the node to deploy into; created if absent"`
	Force bool   `json:"force,omitempty" jsonschema:"adopt a space currently owned by a different stack instead of refusing"`
}

type applyOut struct {
	Space    string   `json:"space"`
	Stack    string   `json:"stack"`
	Result   string   `json:"result"`
	Changes  []string `json:"changes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func (n *node) applyStack(ctx context.Context, in applyInput, r *redactor) (applyOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return applyOut{}, err
	}
	if in.Stack == "" || in.Repo == "" {
		return applyOut{}, errors.New("repo and stack are required")
	}
	repo, err := filepath.Abs(in.Repo)
	if err != nil {
		return applyOut{}, err
	}
	path, err := remoteapply.Resolve(repo, in.Stack)
	if err != nil {
		return applyOut{}, err
	}
	src, err := remoteapply.Source(path, in.Stack, n.warn)
	if err != nil {
		return applyOut{}, err
	}
	for _, v := range src.Vars {
		r.add(v)
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return applyOut{}, err
	}
	res, err := c.ApplySource(ctx, src, in.Force)
	if err != nil {
		return applyOut{}, err
	}
	return applyOut{Space: in.Space, Stack: in.Stack, Result: res.Result, Changes: head(res.Changes, maxChanges), Warnings: head(res.Warnings, maxChanges)}, nil
}

type serviceInput struct {
	Space   string `json:"space" jsonschema:"name of the space on the node"`
	Service string `json:"service" jsonschema:"service name (see deploy_get)"`
}

type jobInput struct {
	Space string `json:"space" jsonschema:"name of the space on the node"`
	Job   string `json:"job" jsonschema:"oneshot job name (see deploy_get)"`
}

type ackOut struct {
	Space   string `json:"space"`
	Service string `json:"service,omitempty"`
	Job     string `json:"job,omitempty"`
	Status  string `json:"status"`
}

func (n *node) restartService(ctx context.Context, in serviceInput, _ *redactor) (ackOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return ackOut{}, err
	}
	if in.Service == "" {
		return ackOut{}, errors.New("service is required")
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return ackOut{}, err
	}
	if err := c.Restart(ctx, in.Service); err != nil {
		return ackOut{}, err
	}
	return ackOut{Space: in.Space, Service: in.Service, Status: "restart requested"}, nil
}

func (n *node) runJob(ctx context.Context, in jobInput, _ *redactor) (ackOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return ackOut{}, err
	}
	if in.Job == "" {
		return ackOut{}, errors.New("job is required")
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return ackOut{}, err
	}
	if err := c.RunJob(ctx, in.Job); err != nil {
		return ackOut{}, err
	}
	return ackOut{Space: in.Space, Job: in.Job, Status: "run requested"}, nil
}

type deleteInput struct {
	Space   string `json:"space" jsonschema:"name of the space to delete"`
	Volumes bool   `json:"volumes,omitempty" jsonschema:"also destroy the space's named volumes and the data in them, irreversibly"`
}

func (n *node) deleteSpace(ctx context.Context, in deleteInput, _ *redactor) (ackOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return ackOut{}, err
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return ackOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if in.Volumes {
		err = c.DownAndDelete(ctx)
	} else {
		err = c.DownAndPurge(ctx)
	}
	if err != nil {
		return ackOut{}, err
	}
	status := "delete requested; named volumes kept"
	if in.Volumes {
		status = "delete requested; named volumes destroyed"
	}
	return ackOut{Space: in.Space, Status: status}, nil
}

type dataStoresInput struct {
	Space string `json:"space,omitempty" jsonschema:"space to inspect; empty lists every space of the node"`
}

type spaceStores struct {
	Space  string           `json:"space"`
	Stores []wire.DataStore `json:"stores,omitempty"`
	Error  string           `json:"error,omitempty"`
}

type dataStoresOut struct {
	Spaces    []spaceStores `json:"spaces"`
	Truncated bool          `json:"truncated,omitempty"`
}

func (n *node) listDataStores(ctx context.Context, in dataStoresInput, r *redactor) (dataStoresOut, error) {
	spaces := []string{in.Space}
	out := dataStoresOut{Spaces: []spaceStores{}}
	if in.Space == "" {
		all, err := n.listSpaces(ctx, noInput{}, r)
		if err != nil {
			return dataStoresOut{}, err
		}
		spaces = spaces[:0]
		for _, s := range all.Spaces {
			spaces = append(spaces, s.Name)
		}
		out.Truncated = all.Truncated
	}
	for _, sp := range spaces {
		row := spaceStores{Space: sp}
		c, err := n.spaceClient(sp)
		if err == nil {
			var res control.DataStoresResponse
			if res, err = c.DataStores(ctx); err == nil {
				row.Stores = head(res.Stores, maxServices)
			}
		}
		if err != nil {
			if in.Space != "" {
				return dataStoresOut{}, err
			}
			row.Error = r.err(err).Error()
		}
		out.Spaces = append(out.Spaces, row)
	}
	return out, nil
}

type deployExecInput struct {
	Space          string   `json:"space" jsonschema:"name of the space on the node"`
	Service        string   `json:"service" jsonschema:"service name (see deploy_get)"`
	Command        []string `json:"command" jsonschema:"argv to run, e.g. [\"sh\",\"-c\",\"env | cut -d= -f1\"]"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"stop waiting after this many seconds (default 60, at most 600)"`
}

func (n *node) execService(ctx context.Context, in deployExecInput, r *redactor) (outputOut, error) {
	if err := requireSpace(in.Space); err != nil {
		return outputOut{}, err
	}
	if in.Service == "" || len(in.Command) == 0 {
		return outputOut{}, errors.New("service and command are required")
	}
	c, err := n.spaceClient(in.Space)
	if err != nil {
		return outputOut{}, err
	}
	wait := in.TimeoutSeconds
	if wait <= 0 {
		wait = defaultExecWait
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(min(wait, maxExecWait))*time.Second)
	defer cancel()
	ring := &lineRing{max: maxOutputLines}
	code, err := c.Exec(ctx, in.Service, in.Command, false, 0, 0, strings.NewReader(""), ring, ring)
	if err != nil {
		return outputOut{}, err
	}
	out, dropped := ring.snapshot()
	return outputOut{OK: code == 0, ExitCode: code, Output: nonNil(out), Truncated: dropped}, nil
}
