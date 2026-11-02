//go:build linux

package rolloutgate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const (
	waitTimeout  = 10 * time.Minute
	pollInterval = 5 * time.Second
)

// ErrNoNode means no deploy context reaches the space. The caller publishes on
// without ordering, which is what a machine with no context always did.
var ErrNoNode = errors.New("no deploy context reaches this space")

// StateReader is the part of the control client Wait polls.
type StateReader func(ctx context.Context) (control.StateResponse, error)

// Applied reports whether the node finished applying image. A non-nil error
// means it never will: the job failed or the rollout was rejected.
func Applied(state control.StateResponse, name, image string) (bool, error) {
	for _, s := range state.Services {
		if s.Name != name {
			continue
		}
		for i := len(s.Runs) - 1; i >= 0; i-- {
			run := s.Runs[i]
			if run.Image != image || run.FinishedAt.IsZero() {
				continue
			}
			if run.ExitCode == 0 && !run.Failed {
				return true, nil
			}
			if s.State == "failed" {
				return false, fmt.Errorf("job %s failed on %s", name, image)
			}
			break
		}
		if s.RejectedReason != "" && s.Image != image {
			return false, fmt.Errorf("%s rejected %s: %s", name, image, s.RejectedReason)
		}
		return len(s.Runs) == 0 && s.Image == image && s.State == "ready", nil
	}
	return false, nil
}

func poll(ctx context.Context, read StateReader, name, image string, timeout, every time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		state, err := read(ctx)
		if err == nil {
			done, failed := Applied(state, name, image)
			if failed != nil {
				return failed
			}
			if done {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not applied within %v: %w", image, timeout, ctx.Err())
		case <-time.After(every):
		}
	}
}

// Wait blocks until the node hosting image's space has applied it.
func (g *Graph) Wait(ctx context.Context, image string, out io.Writer) error {
	t := g.byRepo[ImageRepo(image)]
	if t == nil {
		return nil
	}
	read, err := findNode(ctx, t.space)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[sklp rollout] waiting for %s to apply %s\n", t.name, image)
	if err := poll(ctx, read, t.name, image, waitTimeout, pollInterval); err != nil {
		return err
	}
	fmt.Fprintf(out, "[sklp rollout] %s applied %s\n", t.name, image)
	return nil
}

func findNode(ctx context.Context, space string) (StateReader, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cfg.Contexts[name]
		if c.Server == "" {
			continue
		}
		client, err := control.DialNet(c.Server, config.EffectiveContextToken(c), control.WithCAFingerprint(c.CAFingerprint))
		if err != nil {
			continue
		}
		client = client.WithSpace(space)
		if _, err := client.State(ctx); err == nil {
			return client.State, nil
		}
	}
	return nil, ErrNoNode
}
