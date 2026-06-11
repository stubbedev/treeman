package es

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// TestRestoreChunksOnPairBoundaries shrinks bulkMaxBatch so the dump
// splits across several _bulk POSTs and asserts (a) no chunk ever
// starts with a document line or ends with a dangling action line —
// pairs never straddle a chunk — and (b) every doc arrives exactly
// once across all chunks despite concurrent in-flight POSTs.
func TestRestoreChunksOnPairBoundaries(t *testing.T) {
	old := bulkMaxBatch
	bulkMaxBatch = 96 // force a flush roughly every pair
	defer func() { bulkMaxBatch = old }()

	dir := t.TempDir()
	const docs = 25
	lines := make([]string, 0, 2*docs+1)
	for i := range docs {
		id := strconv.Itoa(i)
		lines = append(lines,
			`{"index":{"_index":"{target_db}items","_id":"`+id+`"}}`,
			`{"n":`+id+`,"pad":"`+strings.Repeat("x", 40)+`"}`,
		)
	}
	// A delete action has no doc line — pairing must not desync on it.
	lines = append(lines, `{"delete":{"_index":"{target_db}items","_id":"gone"}}`)
	p := filepath.Join(dir, "es.ndjson")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu     sync.Mutex
		chunks []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/_bulk":
			chunks = append(chunks, string(body))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"took":1,"errors":false,"items":[]}`))
		case strings.HasSuffix(r.URL.Path, "/_refresh"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.Restore(context.Background(), "wt_a_", p); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chunks) < 2 {
		t.Fatalf("want multiple chunks, got %d", len(chunks))
	}
	seen := 0
	for _, c := range chunks {
		cl := strings.Split(strings.TrimSuffix(c, "\n"), "\n")
		expectDoc := false
		for _, line := range cl {
			if expectDoc {
				if strings.Contains(line, `"_index"`) {
					t.Fatalf("doc position holds an action line: %q in chunk %q", line, c)
				}
				expectDoc = false
				seen++
				continue
			}
			if !strings.HasPrefix(line, `{"index"`) && !strings.HasPrefix(line, `{"delete"`) {
				t.Fatalf("chunk starts mid-pair: %q in chunk %q", line, c)
			}
			expectDoc = strings.HasPrefix(line, `{"index"`)
		}
		if expectDoc {
			t.Fatalf("chunk ends with dangling action line: %q", c)
		}
	}
	if seen != docs {
		t.Fatalf("docs across chunks = %d, want %d", seen, docs)
	}
}
