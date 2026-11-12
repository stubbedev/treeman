package prepare

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for #199: concurrent prepares against one server each ran
// their own fan-out limit, so eight worktrees ran 72 restores at once
// against one mysqld. Sharing CloneSlots holds them to one limit.
func TestCloneSlotsCapRestoresAcrossPrepares(t *testing.T) {
	slots := NewCloneSlots()
	key := serverKey{host: "127.0.0.1", port: 3306}
	opts := RunOptions{CloneSlots: slots}
	const override, prepares, clonesEach = 3, 4, 5

	var running, peak atomic.Int32
	track := func() {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
	}

	var wg sync.WaitGroup
	errs := make(chan error, prepares)
	for i := range prepares {
		wg.Go(func() {
			clones := make([]string, clonesEach)
			for j := range clones {
				clones[j] = fmt.Sprintf("wt%d_t%d", i, j)
			}
			errs <- fanOutClones(context.Background(), nil, 0, 0,
				func(context.Context, string, string) error { track(); return nil },
				"tmpl", clones, "mysql", override, opts.serverBudget(0, key))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := peak.Load(); got > override {
		t.Errorf("peak concurrent restores across prepares = %d, want <= %d", got, override)
	}
}

func TestCloneSlotsSeparateServersDoNotShare(t *testing.T) {
	slots := NewCloneSlots()
	a := slots.server(serverKey{host: "a"})
	b := slots.server(serverKey{host: "b"})
	ctx := context.Background()
	if err := a.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := b.acquire(waitCtx, 1, nil); err != nil {
		t.Fatalf("a full server must not block another server: %v", err)
	}
}

func TestCloneSlotsReportQueueAndAdmission(t *testing.T) {
	s := NewCloneSlots().server(serverKey{host: "h"})
	ctx := context.Background()
	if err := s.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got []CloneProgress
	progress := func(p CloneProgress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}
	admitted := make(chan error, 1)
	go func() { admitted <- s.acquire(ctx, 1, progress) }()

	deadline := time.After(time.Second)
	for {
		mu.Lock()
		queued := len(got) == 1 && got[0] == CloneQueued
		mu.Unlock()
		if queued {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a restore over the limit must report CloneQueued")
		case <-time.After(time.Millisecond):
		}
	}
	s.release()
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []CloneProgress{CloneQueued, CloneDequeued, CloneAdmitted}
	if !slices.Equal(got, want) {
		t.Errorf("progress = %v, want %v", got, want)
	}
}

func TestCloneSlotsAbandonedWaiterFreesItsPlace(t *testing.T) {
	s := NewCloneSlots().server(serverKey{host: "h"})
	ctx := context.Background()
	if err := s.acquire(ctx, 1, nil); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithCancel(ctx)
	var dequeued atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- s.acquire(waitCtx, 1, func(p CloneProgress) {
			if p == CloneDequeued {
				dequeued.Store(true)
			}
		})
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait must fail with the cancellation, got %v", err)
	}
	if !dequeued.Load() {
		t.Error("a cancelled wait must report CloneDequeued")
	}
	s.release()
	nextCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err := s.acquire(nextCtx, 1, nil); err != nil {
		t.Fatalf("the abandoned waiter must not keep the slot: %v", err)
	}
}
