package es

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/reachability"
)

func healthServer(t *testing.T, respond func(call int32, w http.ResponseWriter)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_cluster/health" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if got := r.URL.Query().Get("wait_for_status"); got != "yellow" {
			t.Errorf("wait_for_status = %q, want yellow", got)
		}
		w.Header().Set("Content-Type", "application/json")
		respond(calls.Add(1), w)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func writeHealth(w http.ResponseWriter, code int, status string) {
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"cluster_name":"docker-cluster","status":"` + status + `"}`))
}

// TestConnectWaitsForRecoveringClusterToReachYellow is the #187 ES
// case: a cluster still recovering shards answers RED (HTTP 408 from
// wait_for_status) and a node without a master answers 503; under a
// startup-wait ctx Connect keeps asking until the cluster is yellow.
func TestConnectWaitsForRecoveringClusterToReachYellow(t *testing.T) {
	srv, calls := healthServer(t, func(call int32, w http.ResponseWriter) {
		switch call {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"type":"master_not_discovered_exception"},"status":503}`))
		case 2:
			writeHealth(w, http.StatusRequestTimeout, "red")
		default:
			writeHealth(w, http.StatusOK, "yellow")
		}
	})
	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 10*time.Second)
	defer cancel()
	d, err := Connect(ctx, config.EsConn{URL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if d.Base != srv.URL {
		t.Errorf("Base = %q, want %q", d.Base, srv.URL)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("health calls = %d, want 3 (503, red, yellow)", got)
	}
}

// TestConnectFailsNamingStatusWhenClusterStaysRed: a cluster that is
// still RED when the bound runs out fails with an error naming the
// status rather than a generic timeout.
func TestConnectFailsNamingStatusWhenClusterStaysRed(t *testing.T) {
	srv, _ := healthServer(t, func(_ int32, w http.ResponseWriter) {
		writeHealth(w, http.StatusRequestTimeout, "red")
	})
	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 400*time.Millisecond)
	defer cancel()
	_, err := Connect(ctx, config.EsConn{URL: srv.URL})
	if err == nil {
		t.Fatal("Connect to a RED cluster should fail once the wait runs out")
	}
	msg := err.Error()
	for _, want := range []string{"cluster health is red", "still not ready after waiting", srv.URL} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should contain %q", msg, want)
		}
	}
}

// TestConnectDoesNotRetryHealthAuthFailure: an HTTP error that is not
// a startup state (401) fails on the first health call.
func TestConnectDoesNotRetryHealthAuthFailure(t *testing.T) {
	srv, calls := healthServer(t, func(_ int32, w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing authentication credentials"}`))
	})
	ctx, cancel := context.WithTimeout(reachability.WithStartupWait(context.Background()), 10*time.Second)
	defer cancel()
	_, err := Connect(ctx, config.EsConn{URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("want an HTTP 401 error, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("health calls = %d, want 1", got)
	}
}

// TestConnectWithoutStartupWaitSkipsHealthGate: callers that did not
// opt into the startup wait (doctor, MCP inspection) connect to a RED
// cluster without waiting on its health.
func TestConnectWithoutStartupWaitSkipsHealthGate(t *testing.T) {
	srv, calls := healthServer(t, func(_ int32, w http.ResponseWriter) {
		writeHealth(w, http.StatusRequestTimeout, "red")
	})
	if _, err := Connect(context.Background(), config.EsConn{URL: srv.URL}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("health calls = %d, want 0 without a startup wait", got)
	}
}
