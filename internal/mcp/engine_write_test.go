package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

// TestDBQuery_WriteRequiresAck — the write gate must trip before any
// engine connect: write=true without ack=true is refused up front.
func TestDBQuery_WriteRequiresAck(t *testing.T) {
	_, _, err := dbQueryTool(context.Background(), nil, dbQueryIn{
		Engine: "mysql", DB: "x", Query: "DELETE FROM t", Write: true,
	})
	if err == nil || !strings.Contains(err.Error(), "ack=true") {
		t.Fatalf("expected ack-required error, got %v", err)
	}
}

// TestESRequest_PreflightGates covers the validation + write-gate branches
// that run before loadCfgForRepo (so no live ES is needed).
func TestESRequest_PreflightGates(t *testing.T) {
	ctx := context.Background()

	// Unsupported method.
	if _, _, err := esRequestTool(ctx, nil, esRequestIn{Method: "PATCH", Path: "_cat"}); err == nil {
		t.Errorf("expected unsupported-method error")
	}
	// Empty path.
	if _, _, err := esRequestTool(ctx, nil, esRequestIn{Method: "GET", Path: ""}); err == nil {
		t.Errorf("expected empty-path error")
	}
	// A DELETE (write) without write=true is refused before connecting.
	if _, _, err := esRequestTool(
		ctx,
		nil,
		esRequestIn{Method: "DELETE", Path: "myindex"},
	); err == nil ||
		!strings.Contains(err.Error(), "write") {
		t.Errorf("expected write-gate error for DELETE, got %v", err)
	}
	// write=true but no ack.
	if _, _, err := esRequestTool(
		ctx,
		nil,
		esRequestIn{Method: "PUT", Path: "myindex", Write: true},
	); err == nil ||
		!strings.Contains(err.Error(), "ack=true") {
		t.Errorf("expected ack-required error for PUT, got %v", err)
	}
}

// TestESIsReadRequest classifies method+path into read vs write.
func TestESIsReadRequest(t *testing.T) {
	cases := []struct {
		method, path string
		read         bool
	}{
		{"GET", "_cat/indices?v", true},
		{"HEAD", "myindex", true},
		{"GET", "myindex/_mapping", true},
		{"POST", "myindex/_search", true},
		{"POST", "myindex/_count", true},
		{"POST", "myindex/_doc", false},
		{"POST", "_bulk", false},
		{"PUT", "myindex", false},
		{"DELETE", "myindex/_doc/1", false},
	}
	for _, c := range cases {
		if got := esIsReadRequest(c.method, c.path); got != c.read {
			t.Errorf("esIsReadRequest(%s, %s) = %v, want %v", c.method, c.path, got, c.read)
		}
	}
}

// TestMongoCommand_WriteVerbGate — a write verb in command_json with
// write=false is rejected by verb classification before any connect.
func TestMongoCommand_WriteVerbGate(t *testing.T) {
	// insert is a write verb; the verb classification rejects write=false
	// before any connect, so an empty config is fine here.
	if _, err := dbCommandMongo(context.Background(), &config.Config{}, dbQueryIn{
		DB: "x", CommandJSON: `{"insert":"users","documents":[{"a":1}]}`,
	}); err == nil || !strings.Contains(err.Error(), "write=true") {
		t.Fatalf("expected write-verb gate error, got %v", err)
	}
}
