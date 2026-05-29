package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/runid"
	"github.com/stubbedev/treeman/internal/store"
)

// seedStoreForCompletion creates a temp SQLite store with a known
// shape so each completion source has data to return. Returns the
// db path so callers can point openStore at it via TREEMAN_DB_PATH.
func seedStoreForCompletion(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "t.db")
	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	repoID, _ := s.EnsureRepo(ctx, "/r/foo", "foo")
	_, _ = s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/feature-x", "feature-x", "feat/x")
	_, _ = s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/feature-y", "feature-y", "feat/y")
	_, _ = s.EnsureWorktree(ctx, repoID, "/r/foo/.wt/main", "main", "main")
	// Seed an event with a run_id payload so recentRunIDCompletions
	// has something to extract.
	ctxRid := runid.With(ctx, "abc12345")
	_ = s.WriteEvent(ctxRid, store.LevelInfo, "test_evt", "msg",
		repoID, 0, "", 0, map[string]string{"k": "v"})
	_ = s.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint: "f1", Engine: "mysql", SourceDB: "appdb",
		TemplateName: "tpl", MigrationsHash: "h", LastUsedAt: 100, RepoID: repoID,
	})
	return dbFile
}

func TestCompletionHandler_NilRequestSafe(t *testing.T) {
	res, err := completionHandler(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Completion.Values) != 0 {
		t.Errorf("nil request should yield empty completion, got %#v", res)
	}
}

func TestCompletionHandler_WorktreeArgPullsSlugs(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "diagnose-prepare-failure"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "worktree", Value: ""},
		},
	}
	res, err := completionHandler(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"feature-x": true, "feature-y": true, "main": true}
	for _, v := range res.Completion.Values {
		if !want[v] {
			t.Errorf("unexpected completion %q", v)
		}
		delete(want, v)
	}
	if len(want) > 0 {
		t.Errorf("missing completions: %v", want)
	}
}

func TestCompletionHandler_PrefixFiltersResults(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "diagnose-prepare-failure"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "worktree", Value: "feature"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	for _, v := range res.Completion.Values {
		if v != "feature-x" && v != "feature-y" {
			t.Errorf("prefix filter let %q through", v)
		}
	}
	if len(res.Completion.Values) != 2 {
		t.Errorf("want 2 prefix matches, got %d (%v)", len(res.Completion.Values), res.Completion.Values)
	}
}

func TestCompletionHandler_BranchArgPullsBranches(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "migration-trial"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "branch"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	got := map[string]bool{}
	for _, v := range res.Completion.Values {
		got[v] = true
	}
	for _, b := range []string{"feat/x", "feat/y", "main"} {
		if !got[b] {
			t.Errorf("missing branch %q in completions: %v", b, res.Completion.Values)
		}
	}
}

func TestCompletionHandler_FingerprintFromSnapshots(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "cache-cleanup"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "fingerprint"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	if len(res.Completion.Values) != 1 || res.Completion.Values[0] != "f1" {
		t.Errorf("want [f1], got %v", res.Completion.Values)
	}
}

func TestCompletionHandler_RunIDExtractedFromPayload(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "diagnose-prepare-failure"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "run_id"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	if len(res.Completion.Values) != 1 || res.Completion.Values[0] != "abc12345" {
		t.Errorf("want [abc12345], got %v", res.Completion.Values)
	}
}

func TestCompletionHandler_ResourceTemplateSlug(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref: &mcpsdk.CompleteReference{
				Type: "ref/resource",
				URI:  "treeman://worktrees/{slug}/events",
			},
			Argument: mcpsdk.CompleteParamsArgument{Name: "slug", Value: "main"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	if len(res.Completion.Values) != 1 || res.Completion.Values[0] != "main" {
		t.Errorf("want [main], got %v", res.Completion.Values)
	}
}

func TestCompletionHandler_UnknownArgReturnsEmpty(t *testing.T) {
	t.Setenv("TREEMAN_DB_PATH", seedStoreForCompletion(t))

	req := &mcpsdk.CompleteRequest{
		Params: &mcpsdk.CompleteParams{
			Ref:      &mcpsdk.CompleteReference{Type: "ref/prompt", Name: "anything"},
			Argument: mcpsdk.CompleteParamsArgument{Name: "no_such_arg"},
		},
	}
	res, _ := completionHandler(context.Background(), req)
	if len(res.Completion.Values) != 0 {
		t.Errorf("unknown arg should return empty, got %v", res.Completion.Values)
	}
}

func TestExtractRunID(t *testing.T) {
	cases := map[string]string{
		`{"run_id":"abc","x":"y"}`:      "abc",
		`{"x":"y","run_id":"deadbeef"}`: "deadbeef",
		`{"x":"y"}`:                     "",
		``:                              "",
		`{"run_id":"`:                   "", // no closing quote
	}
	for in, want := range cases {
		if got := extractRunID(in); got != want {
			t.Errorf("extractRunID(%q) = %q, want %q", in, got, want)
		}
	}
}
