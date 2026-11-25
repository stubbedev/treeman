package daemon

import (
	"context"
	"fmt"
	"sync"
)

type lifecycleSlots chan struct{}

func newLifecycleSlots(n uint) lifecycleSlots {
	if n == 0 {
		panic("daemon: lifecycle slots must be at least 1")
	}
	return make(lifecycleSlots, n)
}

func (s lifecycleSlots) acquire(ctx context.Context, onQueued func()) (release func(), err error) {
	select {
	case s <- struct{}{}:
		return s.releaser(), nil
	default:
	}
	onQueued()
	select {
	case s <- struct{}{}:
		return s.releaser(), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for one of the daemon's %d lifecycle slots: %w", cap(s), context.Cause(ctx))
	}
}

func (s lifecycleSlots) releaser() func() {
	return sync.OnceFunc(func() { <-s })
}

func queuedPayload(ctx context.Context, wtPath string) map[string]any {
	payload := map[string]any{"worktree_path": wtPath}
	if t, ok := ctx.Value(triggerKey{}).(trigger); ok {
		payload["trigger"] = t
	}
	return payload
}

func (s lifecycleSlots) queuedMessage() string {
	return fmt.Sprintf("queued: all %d lifecycle slots are busy (daemon.lifecycle_slots)", cap(s))
}
