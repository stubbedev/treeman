package daemon

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/db/reachability"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/store"
)

func refused(engine string) error {
	return &reachability.UnreachableError{Engine: engine, Addr: "127.0.0.1:9200", Reason: reachability.Refused, Err: syscall.ECONNREFUSED}
}

// Regression for #212: every shape of an engine that is down or still
// starting is retried like a refusal; a failure the engine reports is
// not.
func TestCreateRetryForTagsEveryUnreachableShape(t *testing.T) {
	failed := func(err error) error {
		return fmt.Errorf("prepare: %w", &prepare.RunError{Failed: []*prepare.DBError{{Index: 1, Engine: "mysql", Err: err}}})
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			"container stopped",
			&reachability.UnreachableError{Engine: "mysql", Addr: "app-mysql-1", Reason: reachability.ContainerDown},
			true,
		},
		{"connect timeout", &reachability.UnreachableError{Engine: "mysql", Addr: "10.0.0.2:3306", Reason: reachability.TimedOut}, true},
		{"handshake dropped", fmt.Errorf("ping mysql: %w", &reachability.UnreachableError{
			Engine: "mysql", Addr: "127.0.0.1:3306", Reason: reachability.HandshakeDropped, Err: io.EOF,
		}), true},
		{"engine starting", reachability.StartingError("postgres", "127.0.0.1:5432", errors.New("57P03")), true},
		{"authentication failed", errors.New("ping mysql: Error 1045 (28000): Access denied for user 'app'"), false},
		{"wrapped EOF without classification", fmt.Errorf("migrate: %w", io.EOF), false},
	}
	for _, tc := range cases {
		r, ok := createRetryFor(failed(tc.err), 0)
		if ok != tc.want || (ok && (r.Cause != store.RetryEngineUnreachable || !slices.Equal(r.DBs, []int{1}))) {
			t.Errorf("%s: createRetryFor = %+v, %v; want an engine-unreachable retry = %v", tc.name, r, ok, tc.want)
		}
	}
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
