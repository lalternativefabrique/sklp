package sklpmcp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	jobOutputLines = 2000
	maxJobs        = 50
	stopGrace      = 15 * time.Second
)

type job struct {
	id       string
	argv     []string
	dir      string
	started  time.Time
	cmd      *exec.Cmd
	mu       sync.Mutex
	out      *lineRing
	finished *time.Time
	exitCode int
	err      string
}

type jobs struct {
	mu   sync.Mutex
	seq  int
	byID map[string]*job
}

// start runs one sklp invocation in the background. A single run at a time:
// a dev stack and a pipeline each take the whole machine.
func (js *jobs) start(exe, dir string, args ...string) (*job, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.byID {
		if j.running() {
			return nil, fmt.Errorf("job %s (sklp %v) is still running: wait for it (job_status) or stop it (job_cancel)", j.id, j.argv)
		}
	}
	if len(js.byID) >= maxJobs {
		js.dropOldest()
	}
	js.seq++
	j := &job{id: fmt.Sprintf("job-%d", js.seq), argv: args, dir: dir, started: time.Now(), out: &lineRing{max: jobOutputLines}}
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = childEnv()
	cmd.Stdout, cmd.Stderr = j.out, j.out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	j.cmd = cmd
	js.byID[j.id] = j
	go func() {
		err := cmd.Wait()
		now := time.Now()
		j.mu.Lock()
		defer j.mu.Unlock()
		j.finished = &now
		var exitErr *exec.ExitError
		switch {
		case err == nil:
		case errors.As(err, &exitErr):
			j.exitCode = exitErr.ExitCode()
		default:
			j.exitCode = -1
			j.err = err.Error()
		}
	}()
	return j, nil
}

func (js *jobs) dropOldest() {
	var done []*job
	for _, j := range js.byID {
		if !j.running() {
			done = append(done, j)
		}
	}
	sort.Slice(done, func(a, b int) bool { return done[a].started.Before(done[b].started) })
	for _, j := range done[:max(0, len(done)-maxJobs/2)] {
		delete(js.byID, j.id)
	}
}

func (js *jobs) get(id string) (*job, error) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.byID[id]
	if !ok {
		return nil, fmt.Errorf("no job %q (job_status without an id lists them)", id)
	}
	return j, nil
}

func (js *jobs) all() []*job {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := make([]*job, 0, len(js.byID))
	for _, j := range js.byID {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].started.After(out[b].started) })
	return out
}

func (j *job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished == nil
}

// stop asks the run to end the way Ctrl-C would, so sklp tears its spaces
// down itself; the process group is killed only past the grace.
func (j *job) stop(ctx context.Context) {
	if !j.running() {
		return
	}
	_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGINT)
	deadline := time.NewTimer(stopGrace)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for j.running() {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			_ = syscall.Kill(-j.cmd.Process.Pid, syscall.SIGKILL)
			return
		case <-tick.C:
		}
	}
}

type jobOut struct {
	ID         string     `json:"id"`
	Command    []string   `json:"command"`
	Dir        string     `json:"dir,omitempty"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Output     []string   `json:"output,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
}

func (j *job) view(lines int) jobOut {
	j.mu.Lock()
	defer j.mu.Unlock()
	o := jobOut{ID: j.id, Command: append([]string{"sklp"}, j.argv...), Dir: j.dir, StartedAt: j.started, FinishedAt: j.finished, Error: j.err}
	switch {
	case j.finished == nil:
		o.Status = "running"
	case j.exitCode == 0:
		o.Status = "succeeded"
	default:
		o.Status = "failed"
	}
	if j.finished != nil {
		code := j.exitCode
		o.ExitCode = &code
	}
	if lines > 0 {
		all, dropped := j.out.snapshot()
		o.Output = tail(all, lines)
		o.Truncated = dropped || len(all) > lines
	}
	return o
}
