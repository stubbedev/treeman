package reachability

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type expiredUnsignalledCtx struct{ context.Context }

func (expiredUnsignalledCtx) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Millisecond), true
}

func TestProbeCtxReportsAnExpiredDeadlineBeforeItsTimerFires(t *testing.T) {
	ctx := expiredUnsignalledCtx{context.Background()}
	err := ProbeCtx(ctx, "mysql", "198.51.100.1", 3306)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if strings.Contains(err.Error(), "tcp connect timeout") {
		t.Errorf("error %q must not diagnose the caller's deadline as a connect timeout", err)
	}
}

func TestHandshakeErrorLeavesAFailureAfterAnExpiredDeadlineUnchanged(t *testing.T) {
	timeout := os.ErrDeadlineExceeded
	err := HandshakeError(expiredUnsignalledCtx{context.Background()}, "mysql", "h:3306", timeout)
	if _, classified := errors.AsType[*UnreachableError](err); classified || !errors.Is(err, timeout) {
		t.Fatalf("HandshakeError = %v, want the failure unclassified once the caller's deadline passed", err)
	}
}
