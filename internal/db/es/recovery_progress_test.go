package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stubbedev/treeman/internal/progress"
)

// TestWaitForRecoveryReportsAdvances: a clone's shard recovery reports
// progress each time it advances and stays silent while it does not,
// so the finalize stall lease is renewed by a large clone that is
// still copying and not by a wedged one (#209).
func TestWaitForRecoveryReportsAdvances(t *testing.T) {
	readings := []string{
		`{"dst":{"shards":[{"stage":"INDEX","index":{"size":{"recovered_in_bytes":0}}},{"stage":"INDEX","index":{"size":{"recovered_in_bytes":0}}}]}}`,
		`{"dst":{"shards":[{"stage":"INDEX","index":{"size":{"recovered_in_bytes":100}}},{"stage":"INDEX","index":{"size":{"recovered_in_bytes":0}}}]}}`,
		`{"dst":{"shards":[{"stage":"INDEX","index":{"size":{"recovered_in_bytes":100}}},{"stage":"INDEX","index":{"size":{"recovered_in_bytes":0}}}]}}`,
		`{"dst":{"shards":[{"stage":"DONE","index":{"size":{"recovered_in_bytes":100}}},{"stage":"INDEX","index":{"size":{"recovered_in_bytes":0}}}]}}`,
		`{"dst":{"shards":[{"stage":"DONE","index":{"size":{"recovered_in_bytes":100}}},{"stage":"DONE","index":{"size":{"recovered_in_bytes":50}}}]}}`,
	}
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := min(int(polls.Add(1))-1, len(readings)-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(readings[i]))
	}))
	defer srv.Close()

	var reports atomic.Int32
	ctx := progress.With(context.Background(), func() { reports.Add(1) })
	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.waitForRecovery(ctx, "dst"); err != nil {
		t.Fatal(err)
	}
	if got := int(polls.Load()); got != len(readings) {
		t.Errorf("polled %d times, want %d", got, len(readings))
	}
	if got := reports.Load(); got != 2 {
		t.Errorf("reported progress %d times, want 2 (bytes recovered, then a shard done)", got)
	}
}
