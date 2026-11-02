package prepare

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stubbedev/treeman/internal/migrations/runner"
)

// TestRecoverDBIndices: per-database failures stay attributable through
// %w wrapping, so finalize recovery can drop only the
// databases that failed (#119).
func TestRecoverDBIndices(t *testing.T) {
	cause := errors.New("migrate failed")
	joined := &RunError{Failed: []*DBError{
		{Index: 0, Engine: "mysql", Err: cause},
		{Index: 2, Engine: "redis", Err: errors.New("dial")},
	}}
	if !errors.Is(fmt.Errorf("prepare: %w", joined), cause) {
		t.Fatal("RunError must unwrap to each database's cause")
	}
	if got := RecoverDBIndices(fmt.Errorf("prepare: %w", joined)); !slices.Equal(got, []int{0, 2}) {
		t.Fatalf("RecoverDBIndices = %v, want [0 2]", got)
	}
	if got := RecoverDBIndices(errors.New("create-after-engines hook failed")); got != nil {
		t.Fatalf("unattributed error: got %v, want nil", got)
	}
	if got := RecoverDBIndices(nil); got != nil {
		t.Fatalf("nil error: got %v, want nil", got)
	}
}

// TestRecoverDBIndicesSkipsMissingCommand: a database whose step command
// did not resolve was never touched, so recovery leaves it alone (#183).
func TestRecoverDBIndicesSkipsMissingCommand(t *testing.T) {
	missing := &runner.StepError{Label: "migrate", Target: "db", Outcome: runner.Outcome{ExitCode: 127}}
	err := fmt.Errorf("prepare: %w", &RunError{Failed: []*DBError{
		{Index: 1, Engine: "mysql", Err: fmt.Errorf("wrapped: %w", missing)},
		{Index: 3, Engine: "postgres", Err: errors.New("migrate failed")},
	}})
	if got := RecoverDBIndices(err); !slices.Equal(got, []int{3}) {
		t.Fatalf("RecoverDBIndices = %v, want [3]", got)
	}
	onlyMissing := &RunError{Failed: []*DBError{{Index: 1, Engine: "mysql", Err: missing}}}
	if got := RecoverDBIndices(onlyMissing); got == nil || len(got) != 0 {
		t.Fatalf("RecoverDBIndices = %#v, want an empty non-nil slice so nothing is recovered", got)
	}
}
