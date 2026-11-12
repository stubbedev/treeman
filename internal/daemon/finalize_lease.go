package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/progress"
)

// finalizeStalledError is the cancel cause of a finalize the watchdog
// found making no progress for finalizeTimeout.
type finalizeStalledError struct {
	idle time.Duration
}

func (e *finalizeStalledError) Error() string {
	return fmt.Sprintf(
		"finalize made no progress for %s (no phase, hook, dump load, clone restore or shard recovery advanced, none waiting for a clone slot) - child wedged",
		e.idle.Round(time.Second),
	)
}

// stallCause returns the stall that cancelled ctx, or nil when ctx is
// live or was cancelled for another reason (a teardown preempt).
func stallCause(ctx context.Context) *finalizeStalledError {
	if ctx.Err() == nil {
		return nil
	}
	stalled, _ := errors.AsType[*finalizeStalledError](context.Cause(ctx))
	return stalled
}

// leaseState is the watchdog's view of one in-flight finalize. A
// finalize is armed once it holds its cross-process lock and starts
// working; until then (waiting on another process's finalize) it
// cannot stall.
type leaseState struct {
	stall        context.CancelCauseFunc
	lastProgress time.Time
	queuedClones int
	stalledAt    time.Time
	reported     bool
}

// watchdogAction is what FinalizeWatchdogLoop does to one finalize.
type watchdogAction int

const (
	watchdogNone watchdogAction = iota
	// watchdogStall cancels the finalize's work with a stall cause; the
	// finalize records the failure itself once its work returns.
	watchdogStall
	// watchdogReport writes the timeout error for a finalize that has not
	// returned within watchdogInterval of its stall cancel (a phase that
	// ignores its context), so its state stops reading `preparing`.
	watchdogReport
)

// verdict decides the watchdog's action at now. Waiting for a clone slot
// is not a stall: the restores ahead of it are the progress.
func (l leaseState) verdict(now time.Time) watchdogAction {
	switch {
	case l.stall == nil, l.reported:
		return watchdogNone
	case !l.stalledAt.IsZero():
		if now.Sub(l.stalledAt) >= watchdogInterval {
			return watchdogReport
		}
		return watchdogNone
	case l.queuedClones > 0, now.Sub(l.lastProgress) < finalizeTimeout:
		return watchdogNone
	}
	return watchdogStall
}

// finalizeLease is the work context of one in-flight finalize: a child
// of the finalize's preemptible context that the watchdog cancels with
// a finalizeStalledError when the work stops making progress.
type finalizeLease struct {
	st     *State
	wtPath string
	parent context.Context
	ctx    context.Context
	stop   context.CancelCauseFunc
}

// leaseFinalize arms the stall watchdog for the finalize of wtPath and
// returns its lease. The clock starts now: time spent waiting for the
// slot and the cross-process lock does not count. Every step forward
// the work reports through progress.Report on the lease's context
// restarts it. The caller MUST call end.
func (st *State) leaseFinalize(parent context.Context, wtPath string) *finalizeLease {
	parent = progress.With(parent, func() { st.leaseProgressed(wtPath) })
	l := &finalizeLease{st: st, wtPath: wtPath, parent: parent}
	l.renew()
	return l
}

// leaseProgressed restarts the stall clock of the in-flight finalize of
// wtPath. A finalize the watchdog already cancelled stays cancelled.
func (st *State) leaseProgressed(wtPath string) {
	st.updateLease(wtPath, func(ls *leaseState) { ls.lastProgress = time.Now() })
}

// renew replaces a stall-cancelled work context with a fresh one and
// restarts the clock. A step that finished despite the stall cancel
// proved the run was not wedged, so the run continues.
func (l *finalizeLease) renew() context.Context {
	if l.stop != nil {
		l.stop(nil)
	}
	l.ctx, l.stop = context.WithCancelCause(l.parent)
	l.st.updateLease(l.wtPath, func(ls *leaseState) {
		*ls = leaseState{stall: l.stop, lastProgress: time.Now()}
	})
	return l.ctx
}

func (l *finalizeLease) end() {
	l.stop(nil)
}

// annotate adds the stall to err when the lease's work was stalled and
// err does not carry it yet. A nil lease (the run failed before it
// started working) leaves err as is.
func (l *finalizeLease) annotate(err error) error {
	if l == nil {
		return err
	}
	stalled := stallCause(l.ctx)
	if stalled == nil || errors.Is(err, stalled) {
		return err
	}
	return fmt.Errorf("%w: %w", stalled, err)
}

// updateLease applies fn to the lease of the in-flight finalize of
// wtPath, if one is in flight.
func (st *State) updateLease(wtPath string, fn func(*leaseState)) {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	e, ok := st.inFlightFinalizes[wtPath]
	if !ok {
		return
	}
	fn(&e.lease)
	st.inFlightFinalizes[wtPath] = e
}

// finalizeRunOptions are the prepare options of a finalize of wtPath:
// the daemon-wide clone slots, whose queue pauses the lease while a
// restore waits for a slot and whose admissions and finishes renew it.
func (st *State) finalizeRunOptions(wtPath string) prepare.RunOptions {
	return prepare.RunOptions{
		CloneSlots: st.cloneSlots,
		Progress: func(p prepare.CloneProgress) {
			switch p {
			case prepare.CloneQueued:
				st.updateLease(wtPath, func(ls *leaseState) { ls.queuedClones++ })
			case prepare.CloneDequeued:
				st.updateLease(wtPath, func(ls *leaseState) { ls.queuedClones-- })
			case prepare.CloneAdmitted, prepare.CloneFinished:
				st.leaseProgressed(wtPath)
			}
		},
	}
}

// expiredLeases returns the finalizes the watchdog acts on at now, and
// marks them stalled or reported under the same lock so two sweeps never
// act twice.
func (st *State) expiredLeases(now time.Time) map[string]expiredLease {
	st.inFlightMu.Lock()
	defer st.inFlightMu.Unlock()
	out := map[string]expiredLease{}
	for p, e := range st.inFlightFinalizes {
		action := e.lease.verdict(now)
		switch action {
		case watchdogNone:
			continue
		case watchdogStall:
			e.lease.stalledAt = now
		case watchdogReport:
			e.lease.reported = true
		}
		out[p] = expiredLease{action: action, lease: e.lease, startedAt: e.startedAt}
		st.inFlightFinalizes[p] = e
	}
	return out
}

type expiredLease struct {
	action    watchdogAction
	lease     leaseState
	startedAt time.Time
}
