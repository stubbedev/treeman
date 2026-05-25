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
	// Pre-create the destination as a plain directory (NOT a linked
	// worktree).
	wtDir := filepath.Join(repo, ".worktrees", "feature-x")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), CreateRequest{
		RepoRoot: repo,
		Branch:   "feature-x",
	}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "destination path already exists") {
		t.Fatalf("expected 'destination path already exists', got %v", err)
	}
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
