package spacemanager

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	RolloutRolling    = "rolling"
	RolloutSoaking    = "soaking"
	RolloutSucceeded  = "succeeded"
	RolloutRolledBack = "rolled_back"
	RolloutSuperseded = "superseded"

	// defaultSoak is how long the new generation must stay Ready after the
	// cutover before the previous generation is destroyed. A process that binds
	// its port and dies right after passes a TCP probe; only time tells it apart
	// from a healthy one.
	defaultSoak = 60 * time.Second

	defaultProbeInterval = 10 * time.Second
	defaultProbeFailures = 3
	// rolloutCrashLimit ends a rollout as soon as a new instance has crashed
	// this many times before its first Ready, instead of waiting out
	// RolloutTimeout on an image that will never come up.
	rolloutCrashLimit = 3
	// defaultRolloutHistory bounds what a service remembers. Both readers want
	// the recent end of it — the core matches a dispatched roll against the
	// rollout it caused, and the UI shows the last few unsuccessful ones — so
	// a longer tail is only a failed afternoon kept on screen.
	// SKLP_ROLLOUT_HISTORY overrides it when an operator wants more depth.
	defaultRolloutHistory = 5
)

// rolloutHistoryMax is how many rollouts a service keeps, from
// SKLP_ROLLOUT_HISTORY or defaultRolloutHistory. A value below 1 is ignored:
// keeping none would leave a rollback with nothing to explain it.
func rolloutHistoryMax() int {
	if v := os.Getenv("SKLP_ROLLOUT_HISTORY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return defaultRolloutHistory
}

// RolloutRecord is one rolling update of a service and how it ended. The node
// keeps the recent ones per service and serves them on GET /v1/state so a
// caller can tell "this tag was rolled" from "this tag is serving".
type RolloutRecord struct {
	ID         string
	Service    string
	FromImage  string
	ToImage    string
	Result     string
	Reason     string
	ExitCode   int
	LogTail    []string
	StartedAt  time.Time
	FinishedAt time.Time
}

func (a *Agent) soakFor(svc Service) time.Duration {
	if svc.Ready != nil && svc.Ready.Soak != "" {
		if d, err := time.ParseDuration(svc.Ready.Soak); err == nil && d >= 0 {
			return d
		}
	}
	return a.opts.Soak
}

func (st *serviceState) isNewID(id string) bool {
	for _, n := range st.newIDs {
		if n == id {
			return true
		}
	}
	return false
}

func (st *serviceState) rolloutPhase() string {
	if st.soaking {
		return RolloutSoaking
	}
	return RolloutRolling
}

func (st *serviceState) inFlightRecord() RolloutRecord {
	return RolloutRecord{
		ID:        st.rolloutID,
		Service:   st.spec.Name,
		FromImage: st.spec.Image,
		ToImage:   st.newSpec.Image,
		Result:    st.rolloutPhase(),
		StartedAt: st.rolloutStart,
	}
}

// cutoverLocked routes traffic to the new Ready generation and de-routes the
// old one WITHOUT stopping it: the old instances stay alive through the soak so
// a crash of the new generation can be undone by routing back. A job has no
// traffic to soak and completes at once. Caller holds a.mu.
func (a *Agent) cutoverLocked(st *serviceState) {
	svcName := st.spec.Name
	for _, id := range st.oldIDs {
		if inst := a.instances[id]; inst != nil {
			inst.retired = true
		}
	}
	st.backends = nil
	a.syncProxyLocked(svcName)
	a.syncACLLocked()

	soak := a.soakFor(st.newSpec)
	if st.newSpec.Oneshot || soak == 0 {
		a.completeRolloutLocked(st)
		return
	}
	st.soaking = true
	st.soakAt = time.Now().Add(soak)
	a.logger.Printf("service %q: cutover done, soaking for %v before the previous version is stopped", svcName, soak)
}

// completeRolloutLocked promotes the new generation: it becomes the
// steady-state spec and live set, and the old generation is stopped. Caller
// holds a.mu.
func (a *Agent) completeRolloutLocked(st *serviceState) {
	svcName := st.spec.Name
	rec := st.inFlightRecord()
	st.spec = st.newSpec
	for _, id := range st.oldIDs {
		if inst := a.instances[id]; inst != nil {
			if !inst.stopped {
				a.stopInstanceLocked(inst)
			}
			delete(a.instances, id)
		}
	}
	st.liveIDs = append([]string(nil), st.newIDs...)
	st.rejected = nil
	a.resetRolloutLocked(st)
	rec.Result = RolloutSucceeded
	a.recordRolloutLocked(st, rec)
	a.logger.Printf("service %q: rolling update complete (previous version stopped)", svcName)
	a.applyPendingLocked(st)
}

func (a *Agent) applyPendingLocked(st *serviceState) {
	if st.pending == nil {
		return
	}
	next := *st.pending
	st.pending = nil
	if st.rejected != nil && !serviceDiffers(*st.rejected, next) {
		return
	}
	if specChanged(st.spec, next) {
		a.beginRolloutLocked(st, next)
		return
	}
	st.spec = next
}

// abortRolloutLocked ends a rollout without promoting it: the new instances are
// torn down and the previous generation is put (or kept) in front. Inside the
// soak the old instances are still alive and only de-routed, so they are routed
// again; a single-writer service already stopped them and relaunches its
// previous spec. A rolled-back spec is remembered so the same broken spec is not
// re-applied until a different one arrives. Caller holds a.mu.
func (a *Agent) abortRolloutLocked(st *serviceState, result, reason string, exitCode int) {
	svcName := st.spec.Name
	rec := st.inFlightRecord()
	if result == RolloutRolledBack {
		rec.LogTail = a.candidateLogTailLocked(st)
	}
	for _, id := range st.newIDs {
		if inst := a.instances[id]; inst != nil {
			a.transition(inst, Failed, reason)
			inst.retired = true
			a.stopInstanceLocked(inst)
			delete(a.instances, id)
		}
	}
	switch {
	case st.stoppedOld:
		for _, id := range st.oldIDs {
			delete(a.instances, id)
		}
		st.rolloutGen++
		st.liveIDs = st.liveIDs[:0]
		for i := 0; i < st.spec.replicaCount(); i++ {
			id := fmt.Sprintf("%s-v%d-%d", svcName, st.rolloutGen, i)
			st.liveIDs = append(st.liveIDs, id)
			a.instances[id] = a.newInstanceLocked(svcName, st.spec, id)
		}
		a.logger.Printf("service %q: rolling update %s (%s); relaunching the previous version, stopped for the rollout", svcName, result, reason)
	default:
		if st.soaking {
			for _, id := range st.oldIDs {
				if inst := a.instances[id]; inst != nil {
					inst.retired = false
				}
			}
		}
		st.backends = nil
		a.syncProxyLocked(svcName)
		a.gatewayRouteLocked(svcName)
		a.syncACLLocked()
		a.logger.Printf("service %q: rolling update %s (%s); kept the previous version", svcName, result, reason)
	}
	if result == RolloutRolledBack {
		rejected := st.newSpec
		st.rejected = &rejected
	}
	a.resetRolloutLocked(st)
	rec.Result = result
	rec.Reason = reason
	rec.ExitCode = exitCode
	a.recordRolloutLocked(st, rec)
	a.applyPendingLocked(st)
}

func (a *Agent) candidateLogTailLocked(st *serviceState) []string {
	for _, id := range st.newIDs {
		if lines := a.logs.runLines(id); len(lines) > 0 {
			return lines
		}
	}
	return nil
}

func (a *Agent) resetRolloutLocked(st *serviceState) {
	st.rolling = false
	st.soaking = false
	st.newIDs = nil
	st.oldIDs = nil
	st.rolloutAt = time.Time{}
	st.soakAt = time.Time{}
	st.stoppedOld = false
	st.newCrashes = 0
}

func (a *Agent) recordRolloutLocked(st *serviceState, rec RolloutRecord) {
	rec.FinishedAt = time.Now()
	st.rollouts = append(st.rollouts, rec)
	if max := rolloutHistoryMax(); len(st.rollouts) > max {
		st.rollouts = st.rollouts[len(st.rollouts)-max:]
	}
	if a.opts.OnRollout != nil {
		go a.opts.OnRollout(rec)
	}
}

func newRolloutID(svcName string, gen int) string {
	return svcName + "-r" + strconv.Itoa(gen) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
}

// Rollouts returns a service's rollout history, oldest first, with the
// in-flight one last when there is one.
func (a *Agent) Rollouts(name string) []RolloutRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rolloutsLocked(name)
}

func (a *Agent) rolloutsLocked(name string) []RolloutRecord {
	st := a.services[name]
	if st == nil {
		return nil
	}
	out := append([]RolloutRecord(nil), st.rollouts...)
	if st.rolling {
		out = append(out, st.inFlightRecord())
	}
	return out
}

// DesiredServices returns the spec each service is serving right now, in
// desired order. A rollout in flight is not yet the desired spec: until it
// succeeds the previous one is what a restart must relaunch.
func (a *Agent) DesiredServices() []Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Service, 0, len(a.order))
	for _, name := range a.order {
		if st := a.services[name]; st != nil {
			out = append(out, st.spec)
		}
	}
	return out
}
