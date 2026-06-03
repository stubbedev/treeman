package es

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCopyAliasesRewritesPrefixAndPreservesMeta verifies that aliases on
// the source index are re-created on the destination with the prefix
// rewritten into the destination namespace, that alias metadata (filter,
// is_write_index) is carried through, and that an alias not carrying the
// source prefix is skipped to preserve per-worktree isolation.
func TestCopyAliasesRewritesPrefixAndPreservesMeta(t *testing.T) {
	const src = "dev_client_25_category_185_pim_end"
	const dst = "kho_kon_12594_client_25_category_185_pim_end"

	var (
		mu       sync.Mutex
		aliasReq string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_alias"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"` + src + `":{"aliases":{` +
				`"` + src + `_alias":{"is_write_index":true,"filter":{"term":{"x":1}}},` +
				`"global_shared_alias":{}` +
				`}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/_aliases":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			aliasReq = string(body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.copyAliases(context.Background(), src, dst, "dev_", "kho_kon_12594_"); err != nil {
		t.Fatalf("copyAliases: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var parsed struct {
		Actions []struct {
			Add map[string]json.RawMessage `json:"add"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(aliasReq), &parsed); err != nil {
		t.Fatalf("parse /_aliases request %q: %v", aliasReq, err)
	}
	if len(parsed.Actions) != 1 {
		t.Fatalf("want 1 add action (un-prefixed alias skipped), got %d: %s", len(parsed.Actions), aliasReq)
	}
	add := parsed.Actions[0].Add
	if got := string(add["alias"]); got != `"kho_kon_12594_client_25_category_185_pim_end_alias"` {
		t.Errorf("alias name = %s, want rewritten prefix", got)
	}
	if got := string(add["index"]); got != `"`+dst+`"` {
		t.Errorf("index = %s, want %q", got, dst)
	}
	if got := string(add["is_write_index"]); got != "true" {
		t.Errorf("is_write_index = %s, want true (metadata not preserved)", got)
	}
	if _, ok := add["filter"]; !ok {
		t.Errorf("filter metadata dropped: %s", aliasReq)
	}
}

// TestCopyAliasesNoAliasesIsNoop verifies that an index with no aliases
// issues no /_aliases POST.
func TestCopyAliasesNoAliasesIsNoop(t *testing.T) {
	const src = "dev_idx"
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/_aliases" {
			posted = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"` + src + `":{"aliases":{}}}`))
	}))
	defer srv.Close()

	d := &Driver{Base: srv.URL, HTTP: srv.Client()}
	if err := d.copyAliases(context.Background(), src, "kho_x_idx", "dev_", "kho_x_"); err != nil {
		t.Fatalf("copyAliases: %v", err)
	}
	if posted {
		t.Error("posted to /_aliases despite no source aliases")
	}
}
