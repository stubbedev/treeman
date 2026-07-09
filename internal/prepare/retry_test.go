package prepare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestIsTransientConn(t *testing.T) {
	transient := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		errors.New(
			"connection(localhost:27017[-577]) incomplete read of message header: connection closed unexpectedly by the other side: EOF",
		),
		errors.New("batch create tables: driver: bad connection"),
		errors.New("dial tcp: connection refused"),
		fmt.Errorf("wrapped: %w", io.EOF),
	}
	for _, e := range transient {
		if !isTransientConn(e) {
			t.Errorf("expected transient: %v", e)
		}
	}
	notTransient := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		errors.New("index not found with name [foo]"),
		errors.New("syntax error, unexpected token"),
	}
	for _, e := range notTransient {
		if isTransientConn(e) {
			t.Errorf("expected NOT transient: %v", e)
		}
	}
}

func TestRetryTransientRetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := retryTransient(context.Background(), func() error {
		calls++
		if calls < 3 {
			return io.EOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want nil after recovery, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 attempts, got %d", calls)
	}
}

func TestRetryTransientDoesNotRetryCancellation(t *testing.T) {
	calls := 0
	err := retryTransient(context.Background(), func() error {
		calls++
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("cancellation must not retry, got %d attempts", calls)
	}
}

func TestRetryTransientStopsOnCancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := retryTransient(ctx, func() error { calls++; return io.EOF })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("cancelled ctx must short-circuit before fn, got %d", calls)
	}
}

func TestRetryTransientNonTransientNoRetry(t *testing.T) {
	calls := 0
	want := errors.New("index not found")
	err := retryTransient(context.Background(), func() error { calls++; return want })
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("non-transient must not retry: err=%v calls=%d", err, calls)
	}
}

func TestRetryTransientExhaustedAnnotatesEngineDeath(t *testing.T) {
	err := retryTransient(context.Background(), func() error { return io.EOF })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want wrapped io.EOF, got %v", err)
	}
	if !strings.Contains(err.Error(), "died or restarted") {
		t.Fatalf("exhausted transient should hint engine death, got %q", err.Error())
	}
}

func TestAnnotateEngineDeath(t *testing.T) {
	// Non-transient and nil pass through unwrapped (no hint appended).
	plain := errors.New("index not found")
	if got := annotateEngineDeath(plain); strings.Contains(got.Error(), "died or restarted") {
		t.Fatalf("non-transient must pass through unannotated, got %v", got)
	}
	if annotateEngineDeath(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	// Transient gains the hint but still unwraps to the original.
	got := annotateEngineDeath(io.EOF)
	if !errors.Is(got, io.EOF) || !strings.Contains(got.Error(), "died or restarted") {
		t.Fatalf("transient should be annotated + unwrappable, got %v", got)
	}
}
