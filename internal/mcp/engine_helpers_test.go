package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/store"
)

// TestSnapshotLookupByEngineSource_PrefersMostRecentlyUsed seeds two
// snapshots for the same (engine, source_db) pair with different
// last_used_at timestamps and asserts the lookup helper returns the
// most-recent row. This is the slow path the MCP snapshot_inspect
// tool falls back to when the caller doesn't have a fingerprint.
func TestSnapshotLookupByEngineSource_PrefersMostRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	older := store.SnapshotRecord{
		Fingerprint: "old", Engine: "mysql", SourceDB: "app",
		TemplateName: "tpl_old", MigrationsHash: "x", LastUsedAt: 100,
	}
	newer := store.SnapshotRecord{
		Fingerprint: "new", Engine: "mysql", SourceDB: "app",
		TemplateName: "tpl_new", MigrationsHash: "y", LastUsedAt: 200,
	}
	if err := s.RecordSnapshot(ctx, older); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSnapshot(ctx, newer); err != nil {
		t.Fatal(err)
	}

	got, err := snapshotLookupByEngineSource(ctx, s, "mysql", "app")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("want a match, got nil")
	}
	if got.Fingerprint != "new" {
		t.Errorf("want newer fingerprint 'new', got %q", got.Fingerprint)
	}
}

func TestSnapshotLookupByEngineSource_NoMatchReturnsNil(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := snapshotLookupByEngineSource(ctx, s, "mysql", "nope")
	if err != nil {
		t.Fatalf("missing row should be (nil, nil), got err %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %#v", got)
	}
}

// TestHookLogReadTool_TruncatesWhenMaxBytesSet — when the hook log is
// larger than max_bytes, the tool returns the trailing window and
// flags truncated=true. The tail-window behavior is what makes this
// tool worth having over a generic file read.
func TestHookLogReadTool_TruncatesWhenMaxBytesSet(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".treeman-hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(hooksDir, "setup-0.log")
	body := []byte(fmt.Sprintf("HEAD-PADDING-%s\nTAIL", string(make([]byte, 1000))))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	_, out, err := hookLogReadTool(context.Background(), nil, hookLogReadIn{
		WorktreePath: dir,
		Phase:        "setup",
		GroupIdx:     0,
		MaxBytes:     32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated {
		t.Errorf("expected Truncated=true for a 1k file with 32-byte window")
	}
	if len(out.Body) > 32 {
		t.Errorf("body exceeds max_bytes: %d > 32", len(out.Body))
	}
	if out.SizeBytes != int64(len(body)) {
		t.Errorf("size_bytes should be full file size: got %d, want %d", out.SizeBytes, len(body))
	}
}

func TestHookLogReadTool_FullReadWhenMaxBytesZero(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".treeman-hooks")
	_ = os.MkdirAll(hooksDir, 0o755)
	path := filepath.Join(hooksDir, "teardown-2.log")
	body := []byte("complete file body")
	_ = os.WriteFile(path, body, 0o644)

	_, out, err := hookLogReadTool(context.Background(), nil, hookLogReadIn{
		WorktreePath: dir,
		Phase:        "teardown",
		GroupIdx:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated {
		t.Errorf("Truncated=true unexpected on full read")
	}
	if out.Body != string(body) {
		t.Errorf("body mismatch: got %q, want %q", out.Body, string(body))
	}
}

func TestHookLogReadTool_RequiresWorktreeAndPhase(t *testing.T) {
	if _, _, err := hookLogReadTool(context.Background(), nil, hookLogReadIn{}); err == nil {
		t.Errorf("empty args should error")
	}
	if _, _, err := hookLogReadTool(context.Background(), nil, hookLogReadIn{WorktreePath: "/x"}); err == nil {
		t.Errorf("missing phase should error")
	}
}

func TestHookLogReadTool_MissingFileSurfacesStatError(t *testing.T) {
	_, _, err := hookLogReadTool(context.Background(), nil, hookLogReadIn{
		WorktreePath: t.TempDir(),
		Phase:        "setup",
	})
	if err == nil {
		t.Errorf("missing file should error")
	}
}
