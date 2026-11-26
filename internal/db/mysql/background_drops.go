package mysql

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	backgroundDropLimit = 2
	stageDropTimeout    = 30 * time.Second
	dropDrainTimeout    = time.Minute
)

var errBackgroundDropsClosed = errors.New("the driver is closed")

// DropGate admits one background stage drop against a budget shared
// with other work on the server, returning the release of what it
// acquired. It fails only when ctx ends first.
type DropGate func(ctx context.Context) (release func(), err error)

// backgroundDrops runs the drops a finished restore hands off, at most
// cap(slots) at a time and each admitted by gate when one is set, so
// the restore returns without waiting for them. Every started drop runs
// exactly once with ctx, which close cancels once its drain timeout
// passes.
type backgroundDrops struct {
	slots  chan struct{}
	gate   DropGate
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

func newBackgroundDrops(limit int) *backgroundDrops {
	ctx, cancel := context.WithCancel(context.Background())
	return &backgroundDrops{slots: make(chan struct{}, limit), ctx: ctx, cancel: cancel}
}

// start runs drop in the background once a slot is free and the gate
// admits it. A drop still waiting for either when close cancels ctx
// runs at once with the cancelled ctx, so it can report what it leaves
// behind.
func (b *backgroundDrops) start(drop func(context.Context)) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errBackgroundDropsClosed
	}
	b.wg.Go(func() {
		select {
		case b.slots <- struct{}{}:
			defer func() { <-b.slots }()
		case <-b.ctx.Done():
		}
		if b.gate != nil {
			if release, err := b.gate(b.ctx); err == nil {
				defer release()
			}
		}
		drop(b.ctx)
	})
	return nil
}

// close refuses new drops and waits for the started ones, cancelling
// their ctx once timeout passes. It returns only after every started
// drop has returned, so none outlives the caller's resources.
func (b *backgroundDrops) close(timeout time.Duration) {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	timer := time.AfterFunc(timeout, b.cancel)
	defer timer.Stop()
	b.wg.Wait()
	b.cancel()
}
