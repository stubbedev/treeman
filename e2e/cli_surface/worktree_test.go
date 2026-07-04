//go:build e2e

package cli_surface_e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeWorktreeFixture lays down a git repo + .treeman.yaml, registers
// two worktrees in the SQLite DB, and writes a few seed events.
// Returns the repo root and the two worktree paths the tests then
// invoke commands against.
func makeWorktreeFixture(t *testing.T, e *env) (repo, wtA, wtB string) {
	t.Helper()
	repo = newGitRepo(t)
	// `wt delete` against this fixture auto-spawns a daemon that does the
	// git-worktree teardown inside `repo` asynchronously. Stop + drain it
	// before the repo's temp dir is removed (registered after newGitRepo
	// so it runs first under LIFO cleanup) to avoid a RemoveAll race.
	t.Cleanup(func() { stopDaemon(t, e) })
	writeConfig(t, repo, minimalConfig)
	wtA = filepath.Join(repo, ".worktrees", "feat_a")
	wtB = filepath.Join(repo, ".worktrees", "feat_b")
	for _, d := range []string{wtA, wtB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	st := openStore(t, e)
	repoID, err := st.EnsureRepo(ctx, repo, "repo")
	if err != nil {
		t.Fatal(err)
	}
	aID, err := st.EnsureWorktree(ctx, repoID, wtA, "feat_a", "feature/a")
	if err != nil {
		t.Fatal(err)
	}
	bID, err := st.EnsureWorktree(ctx, repoID, wtB, "feat_b", "feature/b")
	if err != nil {
		t.Fatal(err)
	}
	// Seed an event per worktree so `wt show` and `wt logs` have
	// something to print.
	for _, w := range []struct {
		id   int64
		slug string
	}{{aID, "feat_a"}, {bID, "feat_b"}} {
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO events(ts, level, repo_id, worktree_id, event_type, message, payload_json)
			 VALUES (?, 'info', ?, ?, 'fixture_event', ?, '{}')`,
			time.Now().UnixMilli(), repoID, w.id, "seeded for "+w.slug); err != nil {
			t.Fatal(err)
		}
	}
	return repo, wtA, wtB
}

// ── treeman wt list ──────────────────────────────────────────────

func TestWtList(t *testing.T) {
	t.Run("empty registry prints info line, exits 0", func(t *testing.T) {
		repo := newGitRepo(t)
		e := newEnv(t)
		res := e.run(t, repo, "worktree", "list")
		if res.err != nil {
			t.Fatalf("wt list: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// No worktrees → expected to print a "no worktrees" hint
		// rather than an empty table.
		combined := strings.ToLower(res.stdout + res.stderr)
		if !strings.Contains(combined, "no active worktrees") &&
			!strings.Contains(combined, "id") {
			t.Errorf("wt list empty output unexpected:\nstdout:\n%s\nstderr:\n%s",
				res.stdout, res.stderr)
		}
	})

	t.Run("populated registry prints rows", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		res := e.run(t, repo, "worktree", "list")
		if res.err != nil {
			t.Fatalf("wt list: %v\nstderr:\n%s", res.err, res.stderr)
		}
		for _, want := range []string{"feat_a", "feat_b"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("wt list missing %q in:\n%s", want, res.stdout)
			}
		}
	})

	t.Run("--json shape", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		res := e.run(t, repo, "worktree", "list", "--json")
		if res.err != nil {
			t.Fatalf("wt list --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &rows); err != nil {
			t.Fatalf("decode wt list JSON: %v\nstdout:\n%s", err, res.stdout)
		}
		if len(rows) != 2 {
			t.Errorf("expected 2 rows, got %d: %v", len(rows), rows)
		}
	})

	t.Run("--with-state adds STATE column", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		res := e.run(t, repo, "worktree", "list", "--with-state")
		if res.err != nil {
			t.Fatalf("wt list --with-state: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "STATE") {
			t.Errorf("expected STATE header:\n%s", res.stdout)
		}
	})

	t.Run("--sort visited is accepted", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		res := e.run(t, repo, "worktree", "list", "--sort", "visited")
		if res.err != nil {
			t.Errorf("wt list --sort visited: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})

	t.Run("--repo override scopes the listing", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		other := newGitRepo(t)
		res := e.run(t, other, "worktree", "list", "--repo", repo)
		if res.err != nil {
			t.Fatalf("wt list --repo: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "feat_a") {
			t.Errorf("--repo override should surface registered worktrees:\n%s", res.stdout)
		}
	})
}

// ── treeman wt show ──────────────────────────────────────────────

func TestWtShow(t *testing.T) {
	e := newEnv(t)
	repo, _, _ := makeWorktreeFixture(t, e)

	t.Run("by slug surfaces seeded event", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "show", "feat_a")
		if res.err != nil {
			t.Fatalf("wt show: %v\nstderr:\n%s", res.err, res.stderr)
		}
		for _, want := range []string{"feat_a", "fixture_event"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("wt show missing %q:\n%s", want, res.stdout)
			}
		}
	})

	t.Run("by branch", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "show", "feature/b")
		if res.err != nil {
			t.Fatalf("wt show feature/b: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "feat_b") {
			t.Errorf("wt show by branch should resolve to slug:\n%s", res.stdout)
		}
	})

	t.Run("unknown name errors", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "show", "ghost")
		if res.err == nil {
			t.Errorf("expected error for unknown worktree, got stdout:\n%s", res.stdout)
		}
	})

	t.Run("--events / --hooks caps", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "show", "feat_a", "--events", "1", "--hooks", "1")
		if res.err != nil {
			t.Errorf("wt show with caps: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})
}

// ── treeman wt go / back / prev ──────────────────────────────────

func TestWtGoAndBack(t *testing.T) {
	e := newEnv(t)
	repo, wtA, _ := makeWorktreeFixture(t, e)

	t.Run("wt go by name prints path on stdout", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "go", "feat_a")
		if res.err != nil {
			t.Fatalf("wt go: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if strings.TrimSpace(res.stdout) != wtA {
			t.Errorf("wt go stdout = %q, want %q", strings.TrimSpace(res.stdout), wtA)
		}
	})

	t.Run("wt go unknown name errors", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "go", "ghost")
		if res.err == nil {
			t.Errorf("expected error for unknown name, got stdout:\n%s", res.stdout)
		}
	})

	t.Run("wt back from worktree prints main repo path", func(t *testing.T) {
		res := e.run(t, wtA, "worktree", "back")
		if res.err != nil {
			t.Fatalf("wt back: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// In a non-git linked-worktree (fixture path is just a dir
		// under the main repo), "back" resolves through repo-root
		// discovery → the main repo path.
		if !strings.Contains(strings.TrimSpace(res.stdout), repo) {
			t.Errorf("wt back stdout %q should contain repo root %q",
				strings.TrimSpace(res.stdout), repo)
		}
	})
}

func TestWtGoResolveByBranch(t *testing.T) {
	e := newEnv(t)
	repo, wtA, _ := makeWorktreeFixture(t, e)

	t.Run("by branch returns worktree path", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "go", "feature/a")
		if res.err != nil {
			t.Fatalf("wt go: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if strings.TrimSpace(res.stdout) != wtA {
			t.Errorf("wt go = %q, want %q", strings.TrimSpace(res.stdout), wtA)
		}
	})

	t.Run("unknown branch exits nonzero", func(t *testing.T) {
		res := e.run(t, repo, "worktree", "go", "no/such/branch")
		if res.err == nil {
			t.Errorf("expected nonzero exit, got stdout:\n%s", res.stdout)
		}
	})
}

func TestWtPrev(t *testing.T) {
	e := newEnv(t)
	repo, _, _ := makeWorktreeFixture(t, e)

	// With no prior visits there's no "previous" worktree; the command
	// should exit non-zero rather than print a blank path the shell
	// would `cd` into.
	res := e.run(t, repo, "worktree", "prev")
	if res.err == nil && strings.TrimSpace(res.stdout) != "" {
		t.Errorf("expected nonzero exit when no prev worktree, got stdout %q", res.stdout)
	}
}

// ── treeman wt register / unregister ─────────────────────────────

func TestWtRegisterUnregister(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	writeConfig(t, repo, minimalConfig)
	wtPath := filepath.Join(repo, ".worktrees", "manual")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("register adds a row to the registry", func(t *testing.T) {
		res := e.run(t, wtPath, "worktree", "register", "--branch", "manual-branch")
		if res.err != nil {
			t.Fatalf("wt register: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// Confirm via wt list.
		res = e.run(t, repo, "worktree", "list", "--json")
		if res.err != nil {
			t.Fatalf("wt list: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "manual") {
			t.Errorf("registered worktree not in list:\n%s", res.stdout)
		}
	})

	t.Run("unregister marks deleted without touching filesystem", func(t *testing.T) {
		res := e.run(t, wtPath, "worktree", "unregister")
		if res.err != nil {
			t.Fatalf("wt unregister: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if _, err := os.Stat(wtPath); err != nil {
			t.Errorf("wt unregister should leave fs alone, but path is gone: %v", err)
		}
		// Active list should no longer show it.
		res = e.run(t, repo, "worktree", "list", "--json")
		if strings.Contains(res.stdout, `"manual"`) {
			t.Errorf("expected unregistered worktree to drop from active list:\n%s", res.stdout)
		}
	})
}

// ── treeman wt logs (shorthand for `logs tail --worktree`) ──────

func TestWtLogsShorthand(t *testing.T) {
	e := newEnv(t)
	repo, _, _ := makeWorktreeFixture(t, e)
	res := e.run(t, repo, "worktree", "logs", "feat_a")
	if res.err != nil {
		t.Fatalf("wt logs: %v\nstderr:\n%s", res.err, res.stderr)
	}
	if !strings.Contains(res.stdout, "fixture_event") {
		t.Errorf("wt logs shorthand should surface seeded event:\n%s", res.stdout)
	}
}

// ── treeman wt wait (no daemon → timeout error fast) ────────────

func TestWtWait(t *testing.T) {
	e := newEnv(t)
	repo, _, _ := makeWorktreeFixture(t, e)
	// 1s timeout is plenty to confirm exit-nonzero behavior without
	// blocking the test suite. A missing finalize event = the wait
	// should NOT find one in the registry and should report so.
	res := e.run(t, repo, "worktree", "wait", "feat_a", "--timeout", "1s", "--quiet")
	if res.err == nil {
		t.Errorf("expected wt wait to error when no finalize event present, got stdout:\n%s", res.stdout)
	}
}

// ── treeman wt delete dispatches to daemon ───────────────────────

func TestWtDeleteDispatch(t *testing.T) {
	// Engine teardown + git worktree removal are daemon-driven and
	// covered by the cli/ + engine-specific suites. Here we just
	// confirm the CLI surface accepts the command shape and emits the
	// "queued/teardown" status line — that's what the cd-substitution
	// shell shim depends on for the early-return contract.
	e := newEnv(t)
	repo, _, _ := makeWorktreeFixture(t, e)
	res := e.run(t, repo, "worktree", "delete", "--force", "feat_a")
	combined := res.stdout + res.stderr
	if !strings.Contains(combined, "queued") && !strings.Contains(combined, "teardown") &&
		!strings.Contains(combined, "daemon") {
		t.Errorf("wt delete should mention queued/teardown/daemon status:\nstdout:\n%s\nstderr:\n%s",
			res.stdout, res.stderr)
	}
}
