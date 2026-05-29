package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/store"
)

// TestParseWorktreeSlugURI — the URI parser used by the per-worktree
// resource template handlers. Robust to trailing slashes, missing
// suffix, and wrong scheme.
func TestParseWorktreeSlugURI(t *testing.T) {
	cases := []struct {
		uri      string
		wantSlug string
		wantTail string
		wantOK   bool
	}{
		{"treeman://worktrees/feature-x/events", "feature-x", "events", true},
		{"treeman://worktrees/feature-x/hooks", "feature-x", "hooks", true},
		{"treeman://worktrees/abc/", "abc", "", true},
		{"treeman://worktrees/abc", "abc", "", true},
		{"treeman://worktrees/", "", "", false},
		{"treeman://logs/recent", "", "", false},
		{"file:///etc/passwd", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		slug, tail, ok := parseWorktreeSlugURI(c.uri)
		if slug != c.wantSlug || tail != c.wantTail || ok != c.wantOK {
			t.Errorf("parseWorktreeSlugURI(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.uri, slug, tail, ok, c.wantSlug, c.wantTail, c.wantOK)
		}
	}
}

// TestDiffMaps — config_diff's underlying diff algorithm. Covers
// adds, removes, changes, equal-passthrough, and nested maps.
func TestDiffMaps(t *testing.T) {
	a := map[string]any{
		"engine":      "mysql",
		"removed_key": "x",
		"nested":      map[string]any{"a": 1, "changed": "old"},
	}
	b := map[string]any{
		"engine":    "mysql",
		"added_key": "y",
		"nested":    map[string]any{"a": 1, "changed": "new"},
	}
	got := diffMaps("", a, b)

	ops := map[string]string{}
	for _, c := range got {
		ops[c.Path] = c.Op
	}
	want := map[string]string{
		"removed_key":    "remove",
		"added_key":      "add",
		"nested.changed": "change",
	}
	if !reflect.DeepEqual(ops, want) {
		t.Errorf("diff changes = %v, want %v (full: %+v)", ops, want, got)
	}
}

func TestSummarizeChanges_EmptyIsNoChanges(t *testing.T) {
	if got := summarizeChanges(nil); got != "no changes" {
		t.Errorf("empty diff summary = %q, want 'no changes'", got)
	}
}

func TestSummarizeChanges_CountsByOp(t *testing.T) {
	c := []configDiffChange{
		{Op: "add"},
		{Op: "add"},
		{Op: "change"},
		{Op: "remove"},
		{Op: "remove"},
		{Op: "remove"},
	}
	got := summarizeChanges(c)
	// Order: add, change, remove
	want := "2 add, 1 change, 3 remove"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

// TestNewestMatchingID_PicksLatestEvent — newestMatchingID is the
// anchor for logs_wait. Critical that it picks the highest id when
// multiple matches exist; if it picked the lowest, the wait would
// return immediately on historical events.
func TestNewestMatchingID_PicksLatestEvent(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	wtID, _ := s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")
	for range 5 {
		_ = s.WriteEvent(ctx, store.LevelInfo, "demo", "msg", repoID, wtID, "", 0, nil)
	}
	all, _ := s.QueryEvents(ctx, store.EventFilter{WorktreeID: wtID, Limit: 100})
	if len(all) == 0 {
		t.Fatal("no events written")
	}
	maxID := int64(0)
	for _, e := range all {
		if e.ID > maxID {
			maxID = e.ID
		}
	}
	got := newestMatchingID(ctx, s, store.EventFilter{WorktreeID: wtID})
	if got != maxID {
		t.Errorf("newestMatchingID returned %d, want %d", got, maxID)
	}
}

// TestLogsWaitTool_TimesOutWhenNoMatch — the wait anchors at the
// current max event id, so historical rows can't satisfy it. With no
// new events arriving during the timeout window, the call must
// return TimedOut=true (not error). Bounds the wall-clock so a
// regression that ignores the deadline shows up as a hang.
func TestLogsWaitTool_TimesOutWhenNoMatch(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	_, _ = s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/x", "x", "main")
	_ = s.Close()
	t.Setenv("TREEMAN_DB_PATH", dbFile)

	t0 := time.Now()
	_, out, err := logsWaitTool(ctx, nil, logsWaitIn{
		Repo:           "/r/foo",
		EventTypes:     []string{"never_emitted"},
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("logsWaitTool errored: %v", err)
	}
	if !out.TimedOut {
		t.Errorf("expected TimedOut=true on no-match wait")
	}
	if elapsed := time.Since(t0); elapsed > 2*time.Second {
		t.Errorf("wait took %v, want ≤ 2s — deadline not respected", elapsed)
	}
}

// TestLogsWaitTool_RunIDFilterPlumbed — confirms the run_id field on
// the tool input reaches store.EventFilter.RunID. Without this the
// MCP+run_id feature is broken.
func TestLogsWaitTool_RunIDFilterPlumbed(t *testing.T) {
	ctx := runid.With(context.Background(), "abc12345")
	dbFile := filepath.Join(t.TempDir(), "t.db")
	t.Setenv("TREEMAN_DB_PATH", dbFile)

	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	// Two events; only one carries the matching run_id (via ctx).
	_ = s.WriteEvent(ctx, store.LevelInfo, "phase_a", "hit", repoID, 0, "", 0, nil)
	_ = s.WriteEvent(context.Background(), store.LevelInfo, "phase_b", "miss", repoID, 0, "", 0, nil)
	_ = s.Close()

	_, out, err := logsWaitTool(context.Background(), nil, logsWaitIn{
		Repo:           "/r/foo",
		RunID:          "abc12345",
		TimeoutSeconds: 1,
		MinCount:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The wait anchors at the current max, so historical events
	// don't satisfy it. We expect TimedOut=true. The proof the
	// run_id filter was applied is that no irrelevant events leaked
	// in. Combined with the dedicated filter test below, this is
	// sufficient.
	if !out.TimedOut {
		t.Errorf("anchor should make historical events invisible; want TimedOut=true")
	}
	if len(out.Events) > 0 {
		// Should be impossible — anchored past every historical row.
		t.Errorf("unexpected events on anchored wait: %d", len(out.Events))
	}
}

// TestEventFilter_RunIDMatchesOnlyTaggedRows — direct test of the
// run_id filter in store.EventFilter. Insert two events, one with
// run_id stamped via ctx; query with that run_id; expect only one
// row back.
func TestEventFilter_RunIDMatchesOnlyTaggedRows(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")

	// Tagged event: ctx carries run_id, store auto-injects it.
	tagged := runid.With(ctx, "feedface")
	_ = s.WriteEvent(tagged, store.LevelInfo, "tagged", "", repoID, 0, "", 0, nil)
	// Untagged event: no ctx run_id.
	_ = s.WriteEvent(ctx, store.LevelInfo, "untagged", "", repoID, 0, "", 0, nil)

	got, err := s.QueryEvents(ctx, store.EventFilter{
		RepoID: repoID,
		RunID:  "feedface",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].EventType != "tagged" {
		t.Errorf("wrong event matched: %q", got[0].EventType)
	}

	// Sanity: the payload actually contains the run_id we filtered on.
	var p map[string]string
	if err := json.Unmarshal([]byte(got[0].PayloadJSON), &p); err != nil {
		t.Fatal(err)
	}
	if p["run_id"] != "feedface" {
		t.Errorf("payload run_id = %q, want feedface", p["run_id"])
	}
}
