package es

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRestoreSubstitutesTargetDbAndPostsBulk exercises the full
// happy path: a gzipped NDJSON dump with `{target_db}` tokens is
// decompressed, substituted, and POSTed to /_bulk in one chunk; the
// substituted output is verified server-side; the trailing
// _refresh hit confirms we ask ES to make the docs visible.
func TestRestoreSubstitutesTargetDbAndPostsBulk(t *testing.T) {
	dir := t.TempDir()
	dump := strings.Join([]string{
		`{"index":{"_index":"{target_db}products","_id":"1"}}`,
		`{"name":"widget","price":9.99}`,
		`{"index":{"_index":"{target_db}orders","_id":"a"}}`,
		`{"qty":3}`,
	}, "\n") + "\n"
	p := filepath.Join(dir, "es.ndjson.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(dump)); err != nil {
		t.Fatal(err)
	}
	_ = gw.Close()
	_ = f.Close()

	var (
		mu       sync.Mutex
		bulkBody string
		refresh  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/_bulk":
			bulkBody = string(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"took":1,"errors":false,"items":[]}`))
		case strings.HasSuffix(r.URL.Path, "/_refresh"):
			refresh = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.Restore(context.Background(), "wt_alpha_", p); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(bulkBody, `"_index":"wt_alpha_products"`) {
		t.Errorf("token not substituted (products); body=%q", bulkBody)
	}
	if !strings.Contains(bulkBody, `"_index":"wt_alpha_orders"`) {
		t.Errorf("token not substituted (orders); body=%q", bulkBody)
	}
	if strings.Contains(bulkBody, "{target_db}") {
		t.Errorf("literal {target_db} leaked into bulk body: %q", bulkBody)
	}
	if refresh != "/wt_alpha_*/_refresh" {
		t.Errorf("refresh path = %q, want /wt_alpha_*/_refresh", refresh)
	}
}

// TestRestoreFailsOnPerItemErrors asserts we surface a server-
// reported `"errors":true` (e.g. mapping mismatch) instead of
// silently succeeding.
func TestRestoreFailsOnPerItemErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "es.ndjson")
	if err := os.WriteFile(p, []byte(`{"index":{"_index":"x","_id":"1"}}`+"\n"+`{"f":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"took":1,"errors":true,"items":[{"index":{"status":400}}]}`))
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	err := d.Restore(context.Background(), "x", p)
	if err == nil || !strings.Contains(err.Error(), "per-item errors") {
		t.Fatalf("want per-item-errors err, got %v", err)
	}
}
