package prepare

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// transientRetries is how many extra attempts a restore/clone op gets
// when it fails with a transient connection drop (isTransientConn) — the
// signature of an engine that restarted mid-op (a mongod WT_PANIC +
// docker `restart: unless-stopped`, a mysqld reload). Kept small: a
// crash-restarting container is back in ~1s, and a genuinely-down engine
// must not be hammered.
const transientRetries = 3

// transientBackoff is the wait before each retry, indexed by attempt.
// Short, capped rather than unbounded-exponential — the recovery we wait
// on is a process restart (~1s), not load shedding.
var transientBackoff = []time.Duration{250 * time.Millisecond, 1 * time.Second, 2 * time.Second}

// retryTransient runs fn, retrying up to transientRetries times when it
// returns a transient connection error. It never retries a context
// cancellation/deadline — that is a deliberate abort (typically a peer
// engine's failure cancelling the shared errgroup ctx), not a blip. The
// backoff waits are themselves ctx-cancellable.
func retryTransient(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; ; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		err = fn()
		if err == nil || isCancellation(err) || !isTransientConn(err) {
			return err
		}
		if attempt >= transientRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(transientBackoff[min(attempt, len(transientBackoff)-1)]):
		}
	}
}

// isCancellation reports whether err is (or wraps) a context
// cancellation or deadline — i.e. the op was aborted, not failed. Used
// to keep peer-cancellation collateral out of failure counts, warn logs,
// and snapshot eviction.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isTransientConn reports whether err looks like a transient engine
// connection drop. Matched by substring because the driver stack (mongo,
// mysql, pq) has wrapped the underlying net error into an opaque string
// by the time it reaches here.
//
// ponytail: string-match heuristic. A miss costs one un-retried
// transient failure, not a crash — if a new engine surfaces a drop with
// wording not listed here, add it.
func isTransientConn(err error) bool {
	if err == nil || isCancellation(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		"eof",
		"connection closed",
		"connection reset",
		"connection refused",
		"broken pipe",
		"incomplete read",
		"no reachable servers",
		"server selection error",
		"bad connection",
		"i/o timeout",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}
