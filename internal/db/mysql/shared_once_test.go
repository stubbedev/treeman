package mysql

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedOnceRunsOnceForConcurrentCallers(t *testing.T) {
	var s sharedOnce[string]
	var calls atomic.Int32
	gate := make(chan struct{})
	compute := func(context.Context) (string, error) {
		calls.Add(1)
		<-gate
		return "staged", nil
	}
	const callers = 8
	results := make(chan string, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			v, err := s.do(context.Background(), compute)
			if err != nil {
				t.Errorf("do: %v", err)
			}
			results <- v
		})
	}
	close(gate)
	wg.Wait()
	close(results)
	for v := range results {
		if v != "staged" {
			t.Errorf("value = %q, want %q", v, "staged")
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("compute ran %d times, want 1", n)
	}
	if v, ok := s.peek(); !ok || v != "staged" {
		t.Errorf("peek = %q, %v; want %q, true", v, ok, "staged")
	}
}

func TestSharedOnceRerunsAfterTheRunningCallerIsCancelled(t *testing.T) {
	var s sharedOnce[string]
	var calls atomic.Int32
	leaderStarted := make(chan struct{})
	compute := func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(leaderStarted)
			<-ctx.Done()
			return "", errors.New("docker exec: signal: killed")
		}
		return "staged", nil
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := s.do(leaderCtx, compute)
		leaderErr <- err
	}()
	<-leaderStarted

	waiterResult := make(chan string, 1)
	go func() {
		v, err := s.do(context.Background(), compute)
		if err != nil {
			t.Errorf("waiter do: %v", err)
		}
		waiterResult <- v
	}()
	cancelLeader()

	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Errorf("leader err = %v, want it to wrap context.Canceled", err)
	}
	if v := <-waiterResult; v != "staged" {
		t.Errorf("waiter value = %q, want %q: a cancelled run must not be shared", v, "staged")
	}
	v, err := s.do(context.Background(), compute)
	if err != nil || v != "staged" {
		t.Errorf("later do = %q, %v; want the kept %q", v, err, "staged")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("compute ran %d times, want 2", n)
	}
}

func TestSharedOnceWaiterStopsWhenItsOwnContextEnds(t *testing.T) {
	var s sharedOnce[string]
	started := make(chan struct{})
	gate := make(chan struct{})
	compute := func(context.Context) (string, error) {
		close(started)
		<-gate
		return "staged", nil
	}
	leaderResult := make(chan string, 1)
	go func() {
		v, err := s.do(context.Background(), compute)
		if err != nil {
			t.Errorf("leader do: %v", err)
		}
		leaderResult <- v
	}()
	<-started

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWaiter()
	if _, err := s.do(waiterCtx, compute); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiter err = %v, want it to wrap context.DeadlineExceeded", err)
	}

	close(gate)
	if v := <-leaderResult; v != "staged" {
		t.Errorf("leader value = %q, want %q", v, "staged")
	}
	if v, ok := s.peek(); !ok || v != "staged" {
		t.Errorf("peek = %q, %v; want %q, true: a waiter's cancellation must not affect the run", v, ok, "staged")
	}
}

func TestSharedOnceKeepsAFailureNotCausedByCancellation(t *testing.T) {
	var s sharedOnce[string]
	errExport := errors.New("FLUSH TABLES FOR EXPORT: table is not InnoDB")
	var calls atomic.Int32
	compute := func(context.Context) (string, error) {
		calls.Add(1)
		return "", errExport
	}
	for range 2 {
		if _, err := s.do(context.Background(), compute); !errors.Is(err, errExport) {
			t.Errorf("err = %v, want %v", err, errExport)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("compute ran %d times, want 1", n)
	}
	if _, ok := s.peek(); ok {
		t.Error("peek reported a value for a failed computation")
	}
}
