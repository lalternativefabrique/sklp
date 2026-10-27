package plan

// Scheduler invariants (engine 2.5 — issue #388):
//
//   1. Determinism: results are returned in the topo order produced by
//      the compiler (cfg.Plan.CI.Steps), independent of completion time.
//   2. Log coherence: when Jobs > 1 each step's stdout/stderr is
//      captured into per-step buffers and flushed atomically (under a
//      shared mutex) right before the step's OnStepEnd. No lines from
//      step A can ever interleave with lines from step B. When Jobs ==
//      1 we bypass buffering so live progress (spinners, StepProgress
//      hints) keep working exactly as in the pre-2.5 sequential path.
//   3. Failure propagation: a failed step marks the transitive closure
//      of its descendants as "skipped". With FailFast=true, every
//      in-flight step is cancelled via context.
//   4. Cache-key chain: upstreamKey is derived linearly along the topo
//      order so the cache layout is byte-identical to 2.3.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// runScheduler executes every step in cfg.Plan.CI in topo order with
// up to cfg.Jobs concurrent workers. Results are returned in topo order.
func runScheduler(ctx context.Context, cfg CIExecConfig, secretDir string) ([]CIStepResult, error) {
	steps := cfg.Plan.CI.Steps
	n := len(steps)
	results := make([]CIStepResult, n)

	jobs := cfg.Jobs
	if jobs <= 0 {
		jobs = cpuBudget()
	}
	if jobs > n && n > 0 {
		jobs = n
	}
	if jobs < 1 {
		jobs = 1
	}

	// Index steps by name and compute the linear upstream-key chain.
	idx := make(map[string]int, n)
	for i, s := range steps {
		idx[s.Name] = i
	}
	stepKeys := make([]string, n)
	upstreamKey := ""
	for i, s := range steps {
		inputsDigest, derr := ComputeInputsDigest(cfg.Repo, s.ResolvedInputs)
		if derr != nil {
			return nil, fmt.Errorf("inputs digest for step %q: %w", s.Name, derr)
		}
		k, err := ComputeCIStepKey(s, cfg.Plan.CI.Tools, upstreamKey, inputsDigest)
		if err != nil {
			return nil, fmt.Errorf("compute key for step %q: %w", s.Name, err)
		}
		stepKeys[i] = k
		upstreamKey = k
	}

	// In-degree per step (predecessors that still need to complete).
	// Predecessors that were compile-skipped don't gate execution: the
	// runtime treats them as completed.
	inDeg := make([]int, n)
	for i, s := range steps {
		for _, d := range s.DependsOn {
			if _, ok := idx[d]; ok {
				inDeg[i]++
			}
		}
	}

	// Successors from the precomputed adjacency map. We re-derive from
	// DependsOn (rather than Plan.CI.Adjacency) so a plan without an
	// adjacency map still schedules correctly — Adjacency is an
	// optional cache.
	succ := make([][]int, n)
	for i, s := range steps {
		for _, d := range s.DependsOn {
			if pi, ok := idx[d]; ok {
				succ[pi] = append(succ[pi], i)
			}
		}
	}

	// Per-step status used to decide when downstream becomes ready vs
	// gets cascaded as skipped.
	const (
		stPending = iota
		stReady
		stRunning
		stDoneOK   // passed / cached
		stDoneFail // failed (cascade)
		stDoneSkip // skipped (compile or cascade)
	)
	state := make([]int, n)

	// Per-run mutable state shared by workers.
	var (
		mu          sync.Mutex
		flushMu     sync.Mutex // serialises log flushes + OnStepEnd
		readyQueue  []int      // sorted by topo index → deterministic pick order
		inFlight    int
		cond        *sync.Cond
		runCtx      context.Context
		cancel      context.CancelFunc
		failFastHit bool
	)
	cond = sync.NewCond(&mu)
	runCtx, cancel = context.WithCancel(ctx)
	defer cancel()

	// Seed the ready queue and pre-classify compile-skipped steps.
	for i, s := range steps {
		if s.Status == StatusSkippedCompile {
			results[i] = CIStepResult{Name: s.Name, Status: "skipped", SkipReason: s.SkipReason}
			state[i] = stDoneSkip
			// Unblock downstream successors.
			for _, j := range succ[i] {
				inDeg[j]--
			}
			if cfg.OnStepEnd != nil {
				cfg.OnStepEnd(CIStepEvent{Name: s.Name, Status: StatusSkipped, SkipReason: s.SkipReason})
			}
		}
	}
	for i := range steps {
		if state[i] == stDoneSkip {
			continue
		}
		if inDeg[i] == 0 {
			state[i] = stReady
			readyQueue = append(readyQueue, i)
		}
	}

	// markCascade marks the entire downstream closure of `from` as
	// skipped (cascade) and updates results. Caller holds mu.
	var markCascade func(from int, reason string)
	markCascade = func(from int, reason string) {
		for _, j := range succ[from] {
			if state[j] == stDoneSkip || state[j] == stDoneFail || state[j] == stDoneOK {
				continue
			}
			// Pull from ready queue if present.
			for k, idx := range readyQueue {
				if idx == j {
					readyQueue = append(readyQueue[:k], readyQueue[k+1:]...)
					break
				}
			}
			state[j] = stDoneSkip
			results[j] = CIStepResult{Name: steps[j].Name, Status: "skipped", SkipReason: reason}
			// Atomic emit under flushMu so events stay ordered with
			// concurrent finishes.
			if cfg.OnStepEnd != nil {
				flushMu.Lock()
				cfg.OnStepEnd(CIStepEvent{Name: steps[j].Name, Status: StatusSkipped, SkipReason: reason})
				flushMu.Unlock()
			}
			markCascade(j, reason)
		}
	}

	// Worker function. Pulls a ready step, runs it, posts the result.
	worker := func() {
		for {
			mu.Lock()
			for len(readyQueue) == 0 && inFlight > 0 {
				cond.Wait()
			}
			if len(readyQueue) == 0 {
				mu.Unlock()
				return
			}
			i := readyQueue[0]
			readyQueue = readyQueue[1:]
			state[i] = stRunning
			inFlight++
			mu.Unlock()

			step := steps[i]
			stepKey := stepKeys[i]

			// Per-step buffered writers for log coherence. When Jobs == 1
			// we fire OnStepStart inline and write straight through so
			// live progress / spinners work exactly as in the pre-2.5
			// path. When Jobs > 1 we buffer and fire start+flush+end
			// atomically under flushMu so reporters that pin output to a
			// single "current step" writer (see runner/pipeline_exec.go)
			// never see two steps active at once.
			var (
				stdoutW io.Writer = cfg.Stdout
				stderrW io.Writer = cfg.Stderr
				stdoutB *bytes.Buffer
				stderrB *bytes.Buffer
			)
			// Per-step writer routing. The runner's live-reporter path
			// supplies MakeStepWriters so each step's shell stdout/stderr
			// streams straight to dedicated, line-coherent writers --
			// concurrent step output never has to be reassembled on the
			// trunk cfg.Stdout. Falling back to buffered flush keeps the
			// pre-live behaviour for callers that don't.
			usePerStepWriters := cfg.MakeStepWriters != nil
			if usePerStepWriters {
				stdoutW, stderrW = cfg.MakeStepWriters(step.Name)
				if stdoutW == nil {
					stdoutW = cfg.Stdout
				}
				if stderrW == nil {
					stderrW = cfg.Stderr
				}
			} else if jobs > 1 {
				stdoutB = &bytes.Buffer{}
				stderrB = &bytes.Buffer{}
				stdoutW = stdoutB
				stderrW = stderrB
			}
			// Fire OnStepStart inline whenever the reporter can handle
			// concurrent active steps (LiveReporter) OR when there is no
			// risk of overlap (Jobs == 1). Otherwise the start event is
			// deferred to the flushMu critical section so a pre-2.5
			// "single active step" reporter never sees two starts in a
			// row.
			fireStartInline := jobs == 1 || cfg.LiveReporter
			if fireStartInline && cfg.OnStepStart != nil {
				cfg.OnStepStart(CIStepEvent{Name: step.Name})
			}

			out := runStep(runCtx, cfg, step, stepKey, secretDir, stdoutW, stderrW)

			// Atomic emit: (deferred-start) -> flush buffered logs ->
			// end. The flush + end stay serialised so per-step log
			// lines never interleave on cfg.Stdout when the scheduler
			// is the one stitching the trunk output. With LiveReporter
			// the reporter owns concurrency, so OnStepEnd can fire
			// without flushMu — and with MakeStepWriters there is
			// nothing to flush either, so flushMu drops out entirely.
			needsFlushMu := stdoutB != nil || stderrB != nil || !cfg.LiveReporter
			if needsFlushMu {
				flushMu.Lock()
			}
			if !fireStartInline && cfg.OnStepStart != nil {
				cfg.OnStepStart(CIStepEvent{Name: step.Name})
			}
			if stdoutB != nil && cfg.Stdout != nil && stdoutB.Len() > 0 {
				_, _ = cfg.Stdout.Write(stdoutB.Bytes())
			}
			if stderrB != nil && cfg.Stderr != nil && stderrB.Len() > 0 {
				_, _ = cfg.Stderr.Write(stderrB.Bytes())
			}
			if cfg.OnStepEnd != nil {
				cfg.OnStepEnd(out.Event)
			}
			if needsFlushMu {
				flushMu.Unlock()
			}

			res := out.Result
			failed := out.Failed

			mu.Lock()
			results[i] = res
			inFlight--
			if failed {
				state[i] = stDoneFail
				markCascade(i, "previous step failed")
				if cfg.FailFast && !failFastHit {
					failFastHit = true
					cancel()
				}
			} else {
				state[i] = stDoneOK
				// Wake successors whose in-degree drops to zero.
				for _, j := range succ[i] {
					if state[j] != stPending && state[j] != stReady {
						continue
					}
					inDeg[j]--
					if inDeg[j] == 0 && state[j] == stPending {
						state[j] = stReady
						// Insert preserving topo (== ascending index)
						// order so workers pick the same step first on
						// every run for determinism.
						readyQueue = insertSortedInt(readyQueue, j)
					}
				}
			}
			cond.Broadcast()
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()

	// Defensive: any step left in pending here is unreachable from the
	// initial ready set (should not happen since the compiler validates
	// the DAG, but make it observable rather than silent).
	for i, s := range steps {
		if state[i] == stPending || state[i] == stReady {
			results[i] = CIStepResult{
				Name:       s.Name,
				Status:     "skipped",
				SkipReason: "unreachable (no satisfied predecessor)",
			}
		}
	}

	return results, nil
}

// insertSortedInt inserts v into the ascending slice s and returns the
// updated slice.
func insertSortedInt(s []int, v int) []int {
	lo, hi := 0, len(s)
	for lo < hi {
		mid := (lo + hi) / 2
		if s[mid] < v {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	s = append(s, 0)
	copy(s[lo+1:], s[lo:])
	s[lo] = v
	return s
}
