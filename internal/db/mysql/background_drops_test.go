package mysql

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBackgroundDropsStartReturnsWithoutWaitingForTheDrop(t *testing.T) {
	b := newBackgroundDrops(1)
	gate := make(chan struct{})
	finished := make(chan struct{})
	if err := b.start(func(context.Context) {
		<-gate
		close(finished)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		t.Fatal("the drop finished before it was released")
	default:
	}
	close(gate)
	b.close(time.Minute)
	select {
	case <-finished:
	default:
		t.Fatal("close returned before the drop finished")
	}
}

func TestBackgroundDropsRunAtMostLimitAtATime(t *testing.T) {
	const limit, drops = 2, 6
	b := newBackgroundDrops(limit)
	var running, peak, done atomic.Int32
	started := make(chan struct{}, drops)
	gate := make(chan struct{})
	for range drops {
		if err := b.start(func(context.Context) {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			started <- struct{}{}
			<-gate
			running.Add(-1)
			done.Add(1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range limit {
		<-started
	}
	select {
	case <-started:
		t.Fatalf("a drop beyond the limit of %d started while %d were running", limit, limit)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	b.close(time.Minute)
	if p := peak.Load(); p != limit {
		t.Errorf("peak concurrency = %d, want %d", p, limit)
	}
	if n := done.Load(); n != drops {
		t.Errorf("%d drops finished, want %d", n, drops)
	}
}

func TestBackgroundDropsCloseCancelsDropsPastTheDrainTimeout(t *testing.T) {
	b := newBackgroundDrops(1)
	var cancelled atomic.Int32
	running := make(chan struct{})
	if err := b.start(func(ctx context.Context) {
		close(running)
		<-ctx.Done()
		cancelled.Add(1)
	}); err != nil {
		t.Fatal(err)
	}
	<-running
	if err := b.start(func(ctx context.Context) {
		if ctx.Err() != nil {
			cancelled.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	b.close(20 * time.Millisecond)
	if n := cancelled.Load(); n != 2 {
		t.Errorf("%d drops saw the cancelled ctx before close returned, want 2", n)
	}
}

func TestBackgroundDropsRefuseDropsAfterClose(t *testing.T) {
	b := newBackgroundDrops(1)
	b.close(time.Minute)
	ran := false
	if err := b.start(func(context.Context) { ran = true }); !errors.Is(err, errBackgroundDropsClosed) {
		t.Errorf("start after close: err = %v, want %v", err, errBackgroundDropsClosed)
	}
	b.close(time.Minute)
	if ran {
		t.Error("a drop started after close ran")
	}
}

func TestBackgroundDropsWaitForTheGateAndReleaseItAfterTheDrop(t *testing.T) {
	b := newBackgroundDrops(1)
	admit := make(chan struct{})
	var released atomic.Bool
	b.gate = func(ctx context.Context) (func(), error) {
		select {
		case <-admit:
			return func() { released.Store(true) }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	dropped := make(chan bool, 1)
	if err := b.start(func(context.Context) { dropped <- released.Load() }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dropped:
		t.Fatal("the drop ran before the gate admitted it")
	case <-time.After(50 * time.Millisecond):
	}
	close(admit)
	b.close(time.Minute)
	if releasedBeforeDrop := <-dropped; releasedBeforeDrop {
		t.Error("the gate was released before the drop ran")
	}
	if !released.Load() {
		t.Error("the gate was not released after the drop")
	}
}

func TestBackgroundDropsStillRunWhenTheGateIsAbandonedAtClose(t *testing.T) {
	b := newBackgroundDrops(1)
	b.gate = func(ctx context.Context) (func(), error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var sawCancelled atomic.Bool
	if err := b.start(func(ctx context.Context) { sawCancelled.Store(ctx.Err() != nil) }); err != nil {
		t.Fatal(err)
	}
	b.close(20 * time.Millisecond)
	if !sawCancelled.Load() {
		t.Error("the drop waiting on the gate did not run with the cancelled ctx before close returned")
	}
}
