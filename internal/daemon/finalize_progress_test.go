package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/prepare/progress"
)

// Regression for #209: work reporting progress on the lease's context
// (a hook run, a migrate, an Elasticsearch shard recovery) restarts the
// stall clock, so a long phase without clone restores is not cancelled.
func TestReportedProgressRestartsTheClock(t *testing.T) {
	for _, tc := range []struct {
		name      string
		report    bool
		wantStall bool
	}{
		{name: "progress reported", report: true, wantStall: false},
		{name: "no progress", report: false, wantStall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, cleanup := setup(t)
			defer cleanup()
			ctx := context.Background()
			if !st.MarkFinalizeInFlight(armedWt, func() {}) {
				t.Fatal("claim the finalize slot")
			}
			lease := st.leaseFinalize(ctx, armedWt)
			defer lease.end()
			st.updateLease(armedWt, func(ls *leaseState) { ls.lastProgress = time.Now().Add(-2 * finalizeTimeout) })

			if tc.report {
				work, cancel := context.WithCancel(lease.ctx)
				progress.Report(work)
				cancel()
			}
			sweepExpiredFinalizes(ctx, st)

			if stalled := stallCause(lease.ctx) != nil; stalled != tc.wantStall {
				t.Errorf("stalled = %v, want %v", stalled, tc.wantStall)
			}
		})
	}
}
