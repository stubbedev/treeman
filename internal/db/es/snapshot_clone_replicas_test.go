package es

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCloneForcesZeroReplicas asserts the _clone request body pins
// index.number_of_replicas to 0. Inheriting the source's replica count
// (usually 1) on a single-node dev cluster leaves every replica
// unassigned and doubles the shard count toward max_shards_per_node
// until clones fail. Regression for the ES shard explosion.
func TestCloneForcesZeroReplicas(t *testing.T) {
	var cloneBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/_recovery") {
			_, _ = w.Write([]byte(`{"dst_idx":{"shards":[{"stage":"DONE"}]}}`))
			return
		}
		if strings.Contains(r.URL.Path, "/_clone/") {
			b, _ := io.ReadAll(r.Body)
			cloneBody = string(b)
		}
		_, _ = w.Write([]byte(`{"acknowledged":true}`))
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.cloneAPICall(context.Background(), "src_idx", "dst_idx"); err != nil {
		t.Fatalf("cloneAPICall: %v", err)
	}

	var parsed struct {
		Settings map[string]any `json:"settings"`
	}
	if err := json.Unmarshal([]byte(cloneBody), &parsed); err != nil {
		t.Fatalf("clone body not JSON: %q: %v", cloneBody, err)
	}
	got, ok := parsed.Settings["index.number_of_replicas"]
	if !ok {
		t.Fatalf("clone body missing index.number_of_replicas: %q", cloneBody)
	}
	// JSON numbers decode to float64.
	if n, _ := got.(float64); n != 0 {
		t.Errorf("index.number_of_replicas = %v, want 0; body=%q", got, cloneBody)
	}
}
