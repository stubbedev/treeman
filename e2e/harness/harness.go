// Package harness drives docker-compose-backed engine instances for
// treeman's e2e tests. Each subtest brings up its compose file, waits
// for the named service to become reachable, runs assertions through
// treeman's prepare layer, and tears the stack down via t.Cleanup.
//
// Tests run sequentially against the host's docker daemon — compose
// files in this tree bind to known ports for clarity. Run them under
// `go test -tags=e2e ./e2e/...` so they're excluded from the default
// `go test ./...` flow.
package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// ComposeUp runs `docker compose up -d` in dir. The returned func
// brings the stack down and removes the project's named volumes so
// repeat runs start fresh. Failures during teardown are logged but
// not failed — the test result is what matters.
func ComposeUp(t *testing.T, dir string) func() {
	t.Helper()
	cmd := exec.Command("docker", "compose", "up", "-d", "--wait")
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("compose up (%s): %v", dir, err)
	}
	return func() {
		cmd := exec.Command("docker", "compose", "down", "-v", "--remove-orphans")
		cmd.Dir = dir
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Logf("compose down (%s): %v", dir, err)
		}
	}
}

// WaitForReady polls `check` every 500ms until it returns nil or
// timeout elapses. Use to wait for an engine's TCP port to start
// accepting connections — `docker compose up --wait` only guarantees
// the container is RUNNING, not that the engine has finished
// initialising inside it.
func WaitForReady(t *testing.T, label string, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := check(); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s not ready after %s: %v", label, timeout, lastErr)
}

// Env bundles everything a prepare-layer test needs: a writable
// store, a registered repo+worktree, and a synthetic slug.
type Env struct {
	Ctx      context.Context
	Store    *store.Store
	RepoID   int64
	WTID     int64
	Slug     slug.Slug
	RepoPath string
	WTPath   string
}

// NewEnv sets up an in-memory store, registers `wtPath` as both the
// repo and worktree (single-worktree mode is enough for engine
// tests), and returns the bundle. wtPath should already exist on
// disk — usually a t.TempDir().
func NewEnv(t *testing.T, wtPath string) *Env {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "treeman-test.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repoID, err := st.EnsureRepo(ctx, wtPath, filepath.Base(wtPath))
	if err != nil {
		t.Fatalf("ensure repo: %v", err)
	}
	sl := slug.For(wtPath, "main")
	wtID, err := st.EnsureWorktree(ctx, repoID, wtPath, sl.Value, "main")
	if err != nil {
		t.Fatalf("ensure worktree: %v", err)
	}
	return &Env{
		Ctx:      ctx,
		Store:    st,
		RepoID:   repoID,
		WTID:     wtID,
		Slug:     sl,
		RepoPath: wtPath,
		WTPath:   wtPath,
	}
}

// RunPrepare calls prepare.Run against the env's stored worktree.
// Returns the per-DB Outcomes so the test can assert on cache-hit
// status and database/template names.
func (e *Env) RunPrepare(t *testing.T, cfg *config.Config) []prepare.Outcome {
	t.Helper()
	outs, err := prepare.Run(e.Ctx, cfg, e.WTPath, e.Slug, e.Store, e.RepoID, e.WTID, nil)
	if err != nil {
		t.Fatalf("prepare.Run: %v", err)
	}
	return outs
}

// AssertOutcome looks up the per-engine Outcome and reports a clear
// failure when an engine the test expected isn't present.
func AssertOutcome(t *testing.T, outs []prepare.Outcome, engine string, wantCacheHit bool) prepare.Outcome {
	t.Helper()
	for _, o := range outs {
		if o.Engine == engine {
			if o.CacheHit != wantCacheHit {
				t.Errorf("%s: CacheHit=%t, want %t (template=%s)", engine, o.CacheHit, wantCacheHit, o.TemplateName)
			}
			return o
		}
	}
	t.Fatalf("%s: no outcome (got %d outcomes)", engine, len(outs))
	return prepare.Outcome{}
}

// WriteFile is a tiny helper for laying down fixture files inside
// the worktree.
func WriteFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

// SkipIfNoDocker bails out the test when docker isn't on PATH or
// the daemon isn't reachable. Used in TestMain-style guards so
// developers without docker installed see a clean Skip rather than
// a noisy compose error.
func SkipIfNoDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}
}

// MustAbs panics if filepath.Abs fails. Use in test setup where
// the path is hardcoded and abs() can't realistically fail.
func MustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		panic(fmt.Sprintf("abs %s: %v", p, err))
	}
	return abs
}
