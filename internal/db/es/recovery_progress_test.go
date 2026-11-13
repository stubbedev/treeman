package es

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestWaitForRecoveryFailsWhenPrimaryCannotBeAllocated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/_cluster/allocation/explain" {
			_, _ = w.Write([]byte(`{"index":"dst","shard":0,"primary":true,"current_state":"unassigned",` +
				`"can_allocate":"no","allocate_explanation":"Elasticsearch isn't allowed to allocate this shard to any of the nodes in the cluster.",` +
				`"node_allocation_decisions":[{"node_name":"n1","node_decision":"no","deciders":[` +
				`{"decider":"disk_threshold","decision":"NO","explanation":"the node is above the low watermark cluster setting [cluster.routing.allocation.disk.watermark.low=85%]"}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	err := d.waitForRecovery(ctx, "dst")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the allocation failure", err)
	}
	for _, want := range []string{"dst", "disk_threshold", "low watermark"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestWaitForRecoveryKeepsWaitingWhileAllocationIsPending(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/_cluster/allocation/explain" && polls.Load() == 1:
			_, _ = w.Write([]byte(`{"index":"dst","shard":0,"primary":true,"current_state":"unassigned","can_allocate":"throttled"}`))
		case r.URL.Path == "/_cluster/allocation/explain":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"illegal_argument_exception"}}`))
		case polls.Add(1) <= 2:
			_, _ = w.Write([]byte(`{}`))
		default:
			_, _ = w.Write([]byte(`{"dst":{"shards":[{"stage":"DONE","index":{"size":{"recovered_in_bytes":1}}}]}}`))
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.waitForRecovery(ctx, "dst"); err != nil {
		t.Fatalf("throttled or not-yet-visible allocation must keep waiting: %v", err)
	}
}
