package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stubbedev/treeman/internal/store"
)

// bootstrapPromptStub is the minimal GetPromptRequest the prompt
// handler needs. Reused across the bootstrap-* tests.
var bootstrapPromptStub = mcpsdk.GetPromptRequest{
	Params: &mcpsdk.GetPromptParams{
		Name:      "bootstrap-new-repo",
		Arguments: map[string]string{},
	},
}

// extractPromptText pulls the rendered text out of a prompt result.
// Prompts return one user-role TextContent; helpers in prompts.go
// always produce that shape.
func extractPromptText(res *mcpsdk.GetPromptResult) string {
	if res == nil {
		return ""
	}
	for _, msg := range res.Messages {
		if tc, ok := msg.Content.(*mcpsdk.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// newTempRepo creates a bare git-init'd directory under t.TempDir().
// resolveRepo (used by every tool with a `repo` arg) walks up looking
// for a .git or .treeman.yaml; without one the tool errors out long
// before any logic we want to test runs.
func newTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v (%s)", err, out)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	// On macOS the temp dir resolves through /private — match what
	// resolveRepo will see so id lookups don't drift.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// ─── connection_probe ───────────────────────────────────────────────

// TestConnectionProbe_UnsupportedEngine — the engine validator must
// surface an unknown engine as Error, not as a transport-level error.
// Otherwise the agent loses the structured payload it relies on.
func TestConnectionProbe_UnsupportedEngine(t *testing.T) {
	_, out, err := connectionProbeTool(context.Background(), nil, connectionProbeIn{
		Engine: "totallyfake",
	})
	if err != nil {
		t.Fatalf("transport error for unknown engine: %v", err)
	}
	if out.Reachable {
		t.Errorf("expected reachable=false for unknown engine")
	}
	if !strings.Contains(out.Error, "unsupported engine") {
		t.Errorf("expected 'unsupported engine' in error, got %q", out.Error)
	}
}

// TestConnectionProbe_UnreachableHost — a syntactically valid DSN
// pointing at nothing must return reachable=false + an error, not panic.
// 127.0.0.1:1 is closed on every sane machine.
func TestConnectionProbe_UnreachableHost(t *testing.T) {
	_, out, err := connectionProbeTool(context.Background(), nil, connectionProbeIn{
		Engine: "mysql",
		DSN:    "mysql://root:secret@127.0.0.1:1/",
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if out.Reachable {
		t.Errorf("expected reachable=false against 127.0.0.1:1")
	}
	if out.Error == "" {
		t.Errorf("expected non-empty Error on unreachable host")
	}
	if out.Source != "dsn" {
		t.Errorf("Source = %q, want 'dsn'", out.Source)
	}
}

// ─── dry_run paths ──────────────────────────────────────────────────

// TestSnapshotsPurge_DryRunDoesNotMutate — the dry_run path must not
// touch SQLite or the engine. We seed one snapshot row, call purge
// with dry_run=true, then verify the row is still there and
// would_drop=1.
func TestSnapshotsPurge_DryRunDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	repo := newTempRepo(t)
	dbFile := filepath.Join(t.TempDir(), "t.db")
	t.Setenv("TREEMAN_DB_PATH", dbFile)
	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := s.EnsureRepo(ctx, repo, filepath.Base(repo))
	if err := s.RecordSnapshot(ctx, store.SnapshotRecord{
		Fingerprint:  "fp-test",
		Engine:       "mysql",
		SourceDB:     "src",
		TemplateName: "tpl",
		RepoID:       repoID,
		CreatedAt:    1, LastUsedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	_, out, err := snapshotsPurgeTool(ctx, nil, snapshotsPurgeIn{
		Repo:   repo,
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.DryRun {
		t.Errorf("DryRun flag not set in output")
	}
	if out.WouldDrop != 1 {
		t.Errorf("WouldDrop = %d, want 1", out.WouldDrop)
	}
	if out.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0 — dry_run should not mutate", out.Dropped)
	}

	// Verify the snapshot row is still on disk.
	s2, _ := store.Open(ctx, dbFile)
	defer func() { _ = s2.Close() }()
	rid, _ := s2.LookupRepoID(ctx, repo)
	rows, _ := s2.ListSnapshotsForRepo(ctx, rid)
	if len(rows) != 1 {
		t.Errorf("snapshot row was deleted by dry_run: got %d rows, want 1", len(rows))
	}
}

// TestRepoRemove_DryRunCountsRows — repo_remove dry_run returns the
// row counts that WOULD be cascaded. Seed a repo + worktree + event,
// confirm the counts match without removing anything.
func TestRepoRemove_DryRunCountsRows(t *testing.T) {
	ctx := context.Background()
	repo := newTempRepo(t)
	dbFile := filepath.Join(t.TempDir(), "t.db")
	t.Setenv("TREEMAN_DB_PATH", dbFile)
	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := s.EnsureRepo(ctx, repo, filepath.Base(repo))
	_, _ = s.EnsureWorktree(ctx, repoID, filepath.Join(repo, ".wt", "x"), "x", "main")
	_ = s.WriteEvent(ctx, store.LevelInfo, "demo", "msg", repoID, 0, "", 0, nil)
	_ = s.Close()

	_, out, err := repoRemoveTool(ctx, nil, repoRemoveIn{
		Repo:   repo,
		DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.DryRun {
		t.Errorf("DryRun flag not set")
	}
	if out.WouldDrop["worktrees"] != 1 {
		t.Errorf("WouldDrop[worktrees] = %d, want 1", out.WouldDrop["worktrees"])
	}
	if out.WouldDrop["events"] < 1 {
		t.Errorf("WouldDrop[events] = %d, want ≥ 1", out.WouldDrop["events"])
	}

	// Verify the repo row is still on disk.
	s2, _ := store.Open(ctx, dbFile)
	defer func() { _ = s2.Close() }()
	id, _ := s2.LookupRepoID(ctx, repo)
	if id == 0 {
		t.Errorf("dry_run deleted the repo row")
	}
}

// TestWorktreeDelete_DryRunHasPlan — dry_run on worktree_delete
// requires the worktree to resolve via the registry; assert the plan
// returns at least the path. Real per-DB plan content depends on
// .treeman.yaml which we don't have in a unit test, so we focus on
// the resolution + status fields.
func TestWorktreeDelete_DryRunHasPlan(t *testing.T) {
	t.Skip("requires a configured .treeman.yaml + repo on disk; covered by e2e harness")
}

// ─── daemon_state ───────────────────────────────────────────────────

// TestDaemonStateTool_DaemonDown — when treemand isn't running, the
// tool must return status=not-running with an Error, not a transport
// error. Otherwise agents lose the structured payload.
func TestDaemonStateTool_DaemonDown(t *testing.T) {
	// Point at a non-existent socket so rpc.Call fails fast.
	t.Setenv("TREEMAN_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))
	_, out, err := daemonStateTool(context.Background(), nil, daemonStateIn{})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if out.Status != "not-running" {
		t.Errorf("Status = %q, want 'not-running'", out.Status)
	}
	if out.Error == "" {
		t.Errorf("expected non-empty Error when daemon is down")
	}
}

// TestDaemonStateResource_DaemonDown — the resource handler shares
// the tool's collectDaemonState helper; verify it returns valid JSON
// in the not-running state (no error, no panic).
func TestDaemonStateResource_DaemonDown(t *testing.T) {
	t.Setenv("TREEMAN_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))
	out := collectDaemonState(context.Background())
	if out.Status != "not-running" {
		t.Errorf("Status = %q, want 'not-running'", out.Status)
	}
}

// ─── resources: parseRepoResourceURI ────────────────────────────────

// TestParseRepoResourceURI — the resource template parser must accept
// the `cwd` placeholder, reject wrong scheme/suffix, and round-trip
// absolute paths. The `cwd` shorthand is the one users will reach for
// in interactive clients (no URL encoding required).
func TestParseRepoResourceURI(t *testing.T) {
	cases := []struct {
		uri      string
		suffix   string
		wantRepo string
		wantOK   bool
	}{
		{"treeman://repos/cwd/snapshots", "/snapshots", "", true},
		{"treeman://repos/cwd/branches", "/branches", "", true},
		{"treeman://repos/%2Fabs%2Fpath/snapshots", "/snapshots", "%2Fabs%2Fpath", true},
		{"treeman://repos/foo/snapshots", "/wrongsuffix", "", false},
		{"treeman://other/cwd/snapshots", "/snapshots", "", false},
		{"", "/snapshots", "", false},
	}
	for _, c := range cases {
		gotRepo, gotOK := parseRepoResourceURI(c.uri, c.suffix)
		if gotRepo != c.wantRepo || gotOK != c.wantOK {
			t.Errorf("parseRepoResourceURI(%q, %q) = (%q, %v); want (%q, %v)",
				c.uri, c.suffix, gotRepo, gotOK, c.wantRepo, c.wantOK)
		}
	}
}

// ─── worktree_repair (no-config paths) ──────────────────────────────

// TestWorktreeRepair_NoPortsNoSnapshots — most repos start with no
// port slots declared and no cached snapshots. The repair tool must
// surface "skipped" / "ok" actions cleanly (not "fail") so an agent
// running repair on a freshly-enrolled worktree doesn't see spurious
// errors.
func TestWorktreeRepair_NoPortsNoSnapshots(t *testing.T) {
	ctx := context.Background()
	repo := newTempRepo(t)
	dbFile := filepath.Join(t.TempDir(), "t.db")
	t.Setenv("TREEMAN_DB_PATH", dbFile)
	// Point at a non-existent socket so the finalize-dispatch sees
	// the daemon as unreachable (skipped action). Without this the
	// host's running treemand would queue the finalize and the action
	// would come back "fixed".
	t.Setenv("TREEMAN_SOCKET", filepath.Join(t.TempDir(), "no-such.sock"))
	s, err := store.Open(ctx, dbFile)
	if err != nil {
		t.Fatal(err)
	}
	repoID, _ := s.EnsureRepo(ctx, repo, filepath.Base(repo))
	_, _ = s.EnsureWorktree(ctx, repoID, repo, "main", "")
	// Write a minimal .treeman.yaml so resolve.LoadResolved succeeds.
	yaml := []byte("databases: []\n")
	if err := writeFile(filepath.Join(repo, ".treeman.yaml"), yaml); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	_, out, err := worktreeRepairTool(ctx, nil, worktreeRepairIn{
		Repo:     repo,
		Worktree: repo,
	})
	if err != nil {
		t.Fatalf("worktree_repair: %v", err)
	}
	statuses := map[string]string{}
	for _, a := range out.Actions {
		statuses[a.Action] = a.Status
	}
	if statuses["registry"] != "ok" {
		t.Errorf("registry action = %q, want ok (actions: %+v)", statuses["registry"], out.Actions)
	}
	if statuses["ports"] != "skipped" {
		t.Errorf("ports action = %q, want skipped (no slots configured)", statuses["ports"])
	}
	if statuses["snapshots"] != "ok" {
		t.Errorf("snapshots action = %q, want ok (no snapshots cached)", statuses["snapshots"])
	}
	// finalize action: daemon is down in tests, so we expect "skipped".
	if statuses["finalize"] != "skipped" {
		t.Errorf("finalize action = %q, want skipped (daemon unreachable in tests)", statuses["finalize"])
	}
}

// ─── prompts_list ───────────────────────────────────────────────────

// TestPromptsList_ContainsAllRegistered — every prompt in
// registerPrompts must appear in promptsListTool's output, with the
// when-to-use trigger populated. Guards against drift between the
// registration registry and the discovery tool.
func TestPromptsList_ContainsAllRegistered(t *testing.T) {
	_, out, err := promptsListTool(context.Background(), nil, promptsListIn{})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{
		"diagnose-prepare-failure", "scaffold-from-framework", "cache-cleanup",
		"worktree-setup", "migration-trial", "bootstrap-new-repo",
	}
	got := map[string]promptsListEntry{}
	for _, e := range out.Prompts {
		got[e.Name] = e
	}
	for _, name := range expected {
		entry, ok := got[name]
		if !ok {
			t.Errorf("missing prompt %q in promptsListTool output", name)
			continue
		}
		if entry.WhenToUse == "" {
			t.Errorf("prompt %q missing when_to_use trigger phrase", name)
		}
		if entry.Description == "" {
			t.Errorf("prompt %q missing description", name)
		}
	}
}

// ─── engine_logs (no-container path) ────────────────────────────────

// TestEngineLogs_NoContainerRefRejects — when the engine's connection
// block has no container/compose ref, the tool must error with a
// clear message rather than falling through to a docker shell-out
// against an empty container id (which would produce a confusing
// "container ” not found" error).
func TestEngineLogs_NoContainerRefRejects(t *testing.T) {
	repo := newTempRepo(t)
	// .treeman.yaml with a raw-host MySQL connection (no container).
	yaml := []byte("databases: []\nconnections:\n  mysql:\n    host: 127.0.0.1\n    user: root\n")
	if err := writeFile(filepath.Join(repo, ".treeman.yaml"), yaml); err != nil {
		t.Fatal(err)
	}
	_, _, err := engineLogsTool(context.Background(), nil, engineLogsIn{
		Repo:   repo,
		Engine: "mysql",
	})
	if err == nil {
		t.Fatal("expected error for engine without container ref")
	}
	if !strings.Contains(err.Error(), "no container") {
		t.Errorf("error %q should mention 'no container'", err)
	}
}

// writeFile is a t.Fatal-friendly helper for seeding fixture files.
func writeFile(path string, data []byte) error {
	return osWriteFile(path, data, 0o644)
}

// osWriteFile alias to keep the test file's os import optional.
var osWriteFile = os.WriteFile

// ─── elicitation guard ──────────────────────────────────────────────

// TestConfirmDestructive_DryRunShortCircuits — dry_run=true must
// always proceed (the destructive action is a no-op). Guards against
// a regression that asks for confirmation on a dry-run.
func TestConfirmDestructive_DryRunShortCircuits(t *testing.T) {
	ok, reason := confirmDestructive(context.Background(), nil, true, false, "test")
	if !ok || reason != "" {
		t.Errorf("dry_run should proceed; got ok=%v reason=%q", ok, reason)
	}
}

// TestConfirmDestructive_AckSkipsElicit — ack=true means the agent
// has already secured user approval. Must proceed without elicitation
// so non-interactive flows aren't blocked.
func TestConfirmDestructive_AckSkipsElicit(t *testing.T) {
	ok, reason := confirmDestructive(context.Background(), nil, false, true, "test")
	if !ok || reason != "" {
		t.Errorf("ack should proceed; got ok=%v reason=%q", ok, reason)
	}
}

// TestConfirmDestructive_NilSessionFallthroughs — when there is no
// session (e.g. tool called from a test or a one-shot CLI), elicitation
// isn't possible; the helper must fall through to proceed rather than
// refusing every destructive call.
func TestConfirmDestructive_NilSessionFallthroughs(t *testing.T) {
	ok, _ := confirmDestructive(context.Background(), nil, false, false, "test")
	if !ok {
		t.Errorf("nil session should fall through to proceed")
	}
}

// ─── bootstrap-new-repo prompt ──────────────────────────────────────

// TestBootstrapNewRepoPrompt_RendersExpectedTools — the prompt body
// must mention every tool an agent needs to follow the script. Guards
// against a tool rename silently breaking the script.
func TestBootstrapNewRepoPrompt_RendersExpectedTools(t *testing.T) {
	res, err := bootstrapNewRepoPrompt(context.Background(), &bootstrapPromptStub)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || len(res.Messages) == 0 {
		t.Fatal("prompt returned no messages")
	}
	body := extractPromptText(res)
	for _, want := range []string{
		"fw_detect", "init_repo", "schema_install",
		"daemon_status", "registry_register", "prepare_run",
		"connection_probe", "engine_status",
		"diagnose-prepare-failure",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bootstrap-new-repo prompt body missing %q", want)
		}
	}
}
