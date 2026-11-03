package wt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stubbedev/treeman/internal/gitenv"
)

// recordingSink captures every line for assertions.
type recordingSink struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingSink) record(prefix, format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, prefix+": "+fmt.Sprintf(format, args...))
}
func (r *recordingSink) OK(f string, a ...any)   { r.record("OK", f, a...) }
func (r *recordingSink) Warn(f string, a ...any) { r.record("WARN", f, a...) }
func (r *recordingSink) Info(f string, a ...any) { r.record("INFO", f, a...) }

func (r *recordingSink) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	_, err := Create(ctx, CreateRequest{}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "branch is required") {
		t.Fatalf("missing branch should error, got %v", err)
	}
	_, err = Create(ctx, CreateRequest{Branch: "x"}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "repo_root is required") {
		t.Fatalf("missing repo_root should error, got %v", err)
	}
}

func TestCreateExistingPathNonMatchingErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withTempStore(t)
	repo := gitRepo(t, "main")
	// Pre-create the destination as a plain directory holding a file
	// (NOT a git worktree) — the leftover-teardown case. The file makes
	// it possibly-real work, so create must refuse rather than move it.
	wtDir := filepath.Join(repo, ".worktrees", "feature-x")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), CreateRequest{
		RepoRoot: repo,
		Branch:   "feature-x",
	}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("expected 'not a git worktree' for plain dir, got %v", err)
	}

	// A real linked worktree on a DIFFERENT branch still gets the
	// plain "already exists" refusal.
	other := filepath.Join(repo, ".worktrees", "occupied")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-b", "occupied", other).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	_, err = Create(context.Background(), CreateRequest{
		RepoRoot: repo,
		Branch:   "feature-x",
		Path:     other,
	}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "destination path already exists") {
		t.Fatalf("expected 'destination path already exists', got %v", err)
	}
}

func TestCreateRemovesDirOnlyLeftover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withTempStore(t)
	repo := gitRepo(t, "main")
	wtDir := filepath.Join(repo, ".worktrees", "feature-y")
	if err := os.MkdirAll(filepath.Join(wtDir, "docker", "mysql", "conf.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	res, err := Create(context.Background(), CreateRequest{
		RepoRoot:  repo,
		Branch:    "feature-y",
		SkipHooks: true,
	}, sink)
	if err != nil {
		t.Fatalf("Create: %v\nsink:\n%s", err, sink.joined())
	}
	if !gitenv.IsGitWorktree(res.WtPath) {
		t.Errorf("%s is not a git worktree after create", res.WtPath)
	}
	if stale := staleLeftovers(t, filepath.Join(repo, ".worktrees")); len(stale) != 0 {
		t.Errorf("expected the leftover deleted, found %v", stale)
	}
}

func TestCreateMovesAsideUndeletableDirOnlyLeftover(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions this test relies on")
	}
	withTempStore(t)
	repo := gitRepo(t, "main")
	worktrees := filepath.Join(repo, ".worktrees")
	wtDir := filepath.Join(worktrees, "develop")
	if err := os.MkdirAll(filepath.Join(wtDir, "docker", "garage.toml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wtDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, p := range staleLeftovers(t, worktrees) {
			_ = os.Chmod(filepath.Join(worktrees, p), 0o755)
		}
	})
	sink := &recordingSink{}
	res, err := Create(context.Background(), CreateRequest{
		RepoRoot:  repo,
		Branch:    "develop",
		SkipHooks: true,
	}, sink)
	if err != nil {
		t.Fatalf("Create: %v\nsink:\n%s", err, sink.joined())
	}
	if !gitenv.IsGitWorktree(res.WtPath) {
		t.Errorf("%s is not a git worktree after create", res.WtPath)
	}
	if stale := staleLeftovers(t, worktrees); len(stale) != 1 || !strings.HasSuffix(stale[0], "-develop") {
		t.Errorf("expected one stale leftover sibling, got %v", stale)
	}
	if !strings.Contains(sink.joined(), "sudo rm -rf") {
		t.Errorf("expected a removal hint, sink:\n%s", sink.joined())
	}
}

func staleLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), staleLeftoverPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestCreateSkipHooksReturnsNoFinalize(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withTempStore(t)
	repo := gitRepo(t, "main")
	sink := &recordingSink{}
	res, err := Create(context.Background(), CreateRequest{
		RepoRoot:  repo,
		Branch:    "feature-skiphooks",
		SkipHooks: true,
	}, sink)
	if err != nil {
		t.Fatalf("Create: %v\nsink:\n%s", err, sink.joined())
	}
	if res.Status != CreatedNoFinalize {
		t.Errorf("Status = %q, want %q", res.Status, CreatedNoFinalize)
	}
	if res.WtPath == "" {
		t.Error("WtPath empty")
	}
	if res.WorktreeID == 0 {
		t.Error("WorktreeID 0")
	}
	// Verify the worktree directory actually exists.
	if _, err := os.Stat(res.WtPath); err != nil {
		t.Errorf("worktree dir missing: %v", err)
	}
}

func TestCreateIdempotentNoop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withTempStore(t)
	repo := gitRepo(t, "main")
	req := CreateRequest{RepoRoot: repo, Branch: "feature-idem", SkipHooks: true}
	first, err := Create(context.Background(), req, NoopSink{})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if first.Status != CreatedNoFinalize {
		t.Fatalf("first Status = %q, want %q", first.Status, CreatedNoFinalize)
	}
	// Same call again — should detect the existing matching worktree
	// and return a noop.
	second, err := Create(context.Background(), req, NoopSink{})
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if second.Status != CreatedNoop {
		t.Fatalf("second Status = %q, want %q", second.Status, CreatedNoop)
	}
	if second.WtPath != first.WtPath {
		t.Errorf("noop path drift: first=%q second=%q", first.WtPath, second.WtPath)
	}
}

// TestCreateForkedBranchDoesNotTrackBase pins the --no-track rule: a branch
// forked off `origin/<base>` must not adopt it as upstream (or it never reads
// `[gone]` once its own remote branch is merged and deleted, and the merged-
// branch prune + durable reap skip it forever). The base is recorded in git
// config instead. A remote-only checkout (From == Branch) keeps tracking its
// own origin ref.
func TestCreateForkedBranchDoesNotTrackBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	withTempStore(t)
	repo := gitRepo(t, "develop")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Self-remote, so create's pre-fetch resolves the base to origin/develop.
	git("remote", "add", "origin", repo)
	git("branch", "feature-remote")
	git("fetch", "-q", "origin")
	git("branch", "-D", "feature-remote") // now remote-only

	ctx := context.Background()
	if _, err := Create(ctx, CreateRequest{RepoRoot: repo, Branch: "feature-new", SkipHooks: true}, NoopSink{}); err != nil {
		t.Fatalf("Create(feature-new): %v", err)
	}
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--abbrev-ref", "feature-new@{upstream}").CombinedOutput(); err == nil {
		t.Errorf("feature-new must have no upstream, got %q", strings.TrimSpace(string(out)))
	}
	if got := git("config", "--get", "branch.feature-new.treemanBase"); got != "develop" {
		t.Errorf("recorded base = %q, want develop", got)
	}

	if _, err := Create(ctx, CreateRequest{
		RepoRoot: repo, Branch: "feature-remote", From: "feature-remote", SkipHooks: true,
	}, NoopSink{}); err != nil {
		t.Fatalf("Create(feature-remote): %v", err)
	}
	if got := git("rev-parse", "--abbrev-ref", "feature-remote@{upstream}"); got != "origin/feature-remote" {
		t.Errorf("remote-only checkout upstream = %q, want origin/feature-remote", got)
	}
}
