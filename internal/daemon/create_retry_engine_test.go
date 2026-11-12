package daemon

import (
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func refused(engine string) error {
	return fmt.Errorf("%s unreachable at 127.0.0.1:9200 (tcp connect refused: %w)", engine, syscall.ECONNREFUSED)
}

// #187 part 3: a create that failed because an engine was still
// starting is tagged with the databases to wait for, so the daemon can
// re-run it once the engine answers.
func TestCreateRetryForTagsRefusedEngines(t *testing.T) {
	err := fmt.Errorf("prepare: %w", &prepare.RunError{Failed: []*prepare.DBError{
		{Index: 6, Engine: "elasticsearch", Err: refused("elasticsearch")},
	}})
	r, ok := createRetryFor(err, 0)
	if !ok || r.Cause != store.RetryEngineUnreachable || !slices.Equal(r.DBs, []int{6}) {
		t.Fatalf("want an engine-unreachable retry for databases[6], got %+v ok=%v", r, ok)
	}
}

func TestCreateRetryForSkipsMixedPrepareFailures(t *testing.T) {
	err := fmt.Errorf("prepare: %w", &prepare.RunError{Failed: []*prepare.DBError{
		{Index: 0, Engine: "mysql", Err: errors.New("migrate exited 255")},
		{Index: 6, Engine: "elasticsearch", Err: refused("elasticsearch")},
	}})
	if r, ok := createRetryFor(err, 0); ok {
		t.Errorf("a failure a re-run cannot fix must not be retried, got %+v", r)
	}
}

func TestRetryDueKeepsProbingUntilTheWindowCloses(t *testing.T) {
	failedAt := time.Now()
	p := store.PendingCreateRetry{
		Failure: store.Event{Ts: failedAt.UnixMilli()},
		Retry:   store.CreateRetry{Cause: store.RetryEngineUnreachable, DBs: []int{6}},
	}
	if retryDue(p, failedAt.Add(createRetryDelay(0)-time.Second)) {
		t.Error("the first probe waits out the back-off")
	}
	if !retryDue(p, failedAt.Add(createRetryDelay(0)+createRetryProbeWindow-time.Second)) {
		t.Error("an unreachable engine is probed until the window closes")
	}
	if retryDue(p, failedAt.Add(createRetryDelay(0)+createRetryProbeWindow)) {
		t.Error("probing must stop once the window closed")
	}
}
