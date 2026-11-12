package progress

import (
	"context"
	"testing"
)

func TestReportReachesTheInstalledReporter(t *testing.T) {
	calls := 0
	ctx := With(context.Background(), func() { calls++ })
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	Report(child)
	Report(ctx)
	if calls != 2 {
		t.Errorf("reporter called %d times, want 2", calls)
	}
	Report(context.Background())
}
