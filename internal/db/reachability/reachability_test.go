package reachability

import (
	"context"
	"errors"
	"net"
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
	addr := ln.Addr().(*net.TCPAddr)
	if err := ProbeCtx(context.Background(), "test", "127.0.0.1", uint16(addr.Port)); err != nil {
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
	_ = errors.Is // keep import in case future error-wrap changes
}
