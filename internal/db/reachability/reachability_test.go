package reachability

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProbeCtxHonorsCancelledContext is the regression guard for the
// fix that switched probes from "ignore caller ctx, use 1.5s
// internal timeout" to "use caller ctx with DefaultTimeout fallback".
// A pre-cancelled ctx must short-circuit the dial immediately
// instead of waiting out the internal deadline.
func TestProbeCtxHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	// 198.51.100.1 is RFC5737 TEST-NET-2 — guaranteed unroutable
	// from any sane network, so the dial would otherwise wait the
	// full DefaultTimeout (1.5s) before reporting failure.
	err := ProbeCtx(ctx, "mysql", "198.51.100.1", 3306)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from cancelled-ctx probe, got nil")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("cancelled ctx took %v to return (should be near-instant)", elapsed)
	}
}

// TestProbeCtxAppliesDefaultTimeoutWhenNoDeadline asserts the
// fallback: with a deadline-less context, DefaultTimeout still
// caps the dial so callers can't be hung forever by an unrouteable
// host.
func TestProbeCtxAppliesDefaultTimeoutWhenNoDeadline(t *testing.T) {
	start := time.Now()
	err := ProbeCtx(context.Background(), "mysql", "198.51.100.1", 3306)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected probe to fail against unroutable host")
	}
	// Allow a generous margin over DefaultTimeout for slow CI.
	if elapsed > DefaultTimeout+500*time.Millisecond {
		t.Errorf("probe took %v, should cap near DefaultTimeout (%v)", elapsed, DefaultTimeout)
	}
}

// TestProbeCtxSucceedsWhenReachable verifies the happy path against
// an in-process TCP listener.
func TestProbeCtxSucceedsWhenReachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, not *net.TCPAddr", ln.Addr())
	}
	if addr.Port < 0 || addr.Port > 65535 {
		t.Fatalf("listener port %d out of uint16 range", addr.Port)
	}
	port := uint16(addr.Port) //nolint:gosec // addr.Port bounds-checked to 0..65535 above
	if err := ProbeCtx(context.Background(), "test", "127.0.0.1", port); err != nil {
		t.Errorf("probe against in-process listener failed: %v", err)
	}
}

// TestProbeCtxErrorMessageNamesEngine ensures the surfaced error
// includes the engine label so users don't have to guess which
// service is unreachable.
func TestProbeCtxErrorMessageNamesEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := ProbeCtx(ctx, "mongodb", "198.51.100.1", 27017)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "mongodb") {
		t.Errorf("error %q should mention engine name", err)
	}
}

func closedLoopbackPort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr is %T, not *net.TCPAddr", ln.Addr())
	}
	_ = ln.Close()
	return uint16(addr.Port) //nolint:gosec // a TCP listener port always fits uint16
}

// TestProbeCtxRefusedOnLoopbackSaysEngineNotListening is the #187 hint
// guard: a refused dial on loopback means the engine is down or still
// starting, so the error must say that and point at `treeman doctor`
// instead of suggesting `container:`/`compose_service:`.
func TestProbeCtxRefusedOnLoopbackSaysEngineNotListening(t *testing.T) {
	err := ProbeCtx(context.Background(), "elasticsearch", "127.0.0.1", closedLoopbackPort(t))
	if err == nil {
		t.Fatal("expected refused probe to fail")
	}
	if !IsRefused(err) {
		t.Fatalf("IsRefused(%v) = false, want true", err)
	}
	msg := err.Error()
	for _, want := range []string{"refused", "down or still starting", "treeman doctor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "compose_service") {
		t.Errorf("error %q should not suggest compose_service for a loopback refusal", msg)
	}
}

var errStarting = errors.New("starting")

func isStarting(err error) bool { return errors.Is(err, errStarting) }

// TestWaitForWithoutStartupWaitTriesOnce: an unmarked ctx (doctor,
// MCP probes) gets exactly one attempt, so diagnostics fail fast.
func TestWaitForWithoutStartupWaitTriesOnce(t *testing.T) {
	attempts := 0
	err := WaitFor(context.Background(), func(context.Context) error {
		attempts++
		return errStarting
	}, isStarting)
	if !errors.Is(err, errStarting) {
		t.Fatalf("err = %v, want errStarting", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestWaitForRetriesUntilReady: under WithStartupWait a retryable
// failure is retried until the attempt succeeds.
func TestWaitForRetriesUntilReady(t *testing.T) {
	attempts := 0
	err := WaitFor(WithStartupWait(context.Background()), func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errStarting
		}
		return nil
	}, isStarting)
	if err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

// TestWaitForReturnsNonRetryableImmediately: an error the predicate
// rejects ends the wait on the first attempt.
func TestWaitForReturnsNonRetryableImmediately(t *testing.T) {
	permanent := errors.New("bad credentials")
	attempts := 0
	err := WaitFor(WithStartupWait(context.Background()), func(context.Context) error {
		attempts++
		return permanent
	}, isStarting)
	if !errors.Is(err, permanent) {
		t.Fatalf("err = %v, want the permanent error", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestWaitForGivesUpAtDeadline: the wait is bounded by the caller's
// deadline and reports the last retryable error with how long it
// waited.
func TestWaitForGivesUpAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(WithStartupWait(context.Background()), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := WaitFor(ctx, func(context.Context) error { return errStarting }, isStarting)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WaitFor took %v, should stop at the 300ms deadline", elapsed)
	}
	if !errors.Is(err, errStarting) {
		t.Fatalf("err = %v, want it to wrap errStarting", err)
	}
	if !strings.Contains(err.Error(), "still not ready after waiting") {
		t.Errorf("error %q should say how long it waited", err)
	}
}

// TestWaitForProbeSeesEngineStartLate is the #187 scenario end to end
// at the TCP layer: the port refuses at first and starts listening a
// moment later; the wait turns that into a delay instead of a failure.
func TestWaitForProbeSeesEngineStartLate(t *testing.T) {
	port := closedLoopbackPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	listening := make(chan net.Listener, 1)
	go func() {
		time.Sleep(400 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			listening <- nil
			return
		}
		listening <- ln
	}()
	ctx, cancel := context.WithTimeout(WithStartupWait(context.Background()), 10*time.Second)
	defer cancel()
	err := WaitFor(ctx, func(ctx context.Context) error {
		return ProbeCtx(ctx, "elasticsearch", "127.0.0.1", port)
	}, IsRefused)
	ln := <-listening
	if ln == nil {
		t.Skipf("port %d was taken before the late listener could bind", port)
	}
	defer func() { _ = ln.Close() }()
	if err != nil {
		t.Fatalf("WaitFor should succeed once the listener is up: %v", err)
	}
}
