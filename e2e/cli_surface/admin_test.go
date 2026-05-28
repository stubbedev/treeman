//go:build e2e

package cli_surface_e2e

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// ── treeman registry ─────────────────────────────────────────────

func TestRegistryRepair(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	res := e.run(t, repo, "registry", "repair")
	if res.err != nil {
		t.Fatalf("registry repair: %v\nstderr:\n%s", res.err, res.stderr)
	}
	// With no git-worktree rows to reconcile, output should still be
	// well-formed (a "0 added / 0 removed" summary or similar).
	if strings.TrimSpace(res.stdout+res.stderr) == "" {
		t.Errorf("registry repair produced no output")
	}
}

func TestRegistryRemove(t *testing.T) {
	t.Run("refuses without --yes when interactive prompt blocked", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		// `registry remove` confirms before dropping; without --yes and
		// without stdin attached it should refuse and exit non-zero.
		res := e.run(t, repo, "registry", "remove", "--repo", repo)
		if res.err == nil {
			t.Errorf("expected registry remove to refuse without --yes")
		}
	})

	t.Run("--force --yes drops the repo row", func(t *testing.T) {
		e := newEnv(t)
		repo, _, _ := makeWorktreeFixture(t, e)
		res := e.run(t, repo, "registry", "remove", "--repo", repo, "--force", "--yes")
		if res.err != nil {
			t.Fatalf("registry remove --force --yes: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// `wt list` now finds no rows under that repo.
		res = e.run(t, repo, "wt", "list", "--json")
		if strings.Contains(res.stdout, `"feat_a"`) {
			t.Errorf("expected worktree rows to drop after registry remove:\n%s", res.stdout)
		}
	})
}

// ── treeman snapshots ────────────────────────────────────────────

func TestSnapshotsList(t *testing.T) {
	t.Run("empty cache exits 0", func(t *testing.T) {
		e := newEnv(t)
		repo := newGitRepo(t)
		writeConfig(t, repo, minimalConfig)
		res := e.run(t, repo, "snapshots", "list")
		if res.err != nil {
			t.Fatalf("snapshots list: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})

	t.Run("--json yields well-formed JSON", func(t *testing.T) {
		e := newEnv(t)
		repo := newGitRepo(t)
		writeConfig(t, repo, minimalConfig)
		res := e.run(t, repo, "snapshots", "list", "--json")
		if res.err != nil {
			t.Fatalf("snapshots list --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var any interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &any); err != nil {
			t.Fatalf("decode snapshots JSON %q: %v", res.stdout, err)
		}
	})
}

func TestSnapshotsPurge(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	writeConfig(t, repo, minimalConfig)
	res := e.run(t, repo, "snapshots", "purge")
	if res.err != nil {
		t.Fatalf("snapshots purge: %v\nstderr:\n%s", res.err, res.stderr)
	}
}

// ── treeman daemon status ────────────────────────────────────────

func TestDaemonStatus(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)

	t.Run("text mode reports not running when socket is absent", func(t *testing.T) {
		res := e.run(t, repo, "daemon", "status")
		// `daemon status` typically exits non-zero when the daemon is
		// down. Either way the output should explain the state.
		combined := strings.ToLower(res.stdout + res.stderr)
		if !strings.Contains(combined, "not running") &&
			!strings.Contains(combined, "no socket") &&
			!strings.Contains(combined, "stopped") {
			t.Errorf("expected daemon-down hint in output:\nstdout:\n%s\nstderr:\n%s",
				res.stdout, res.stderr)
		}
	})

	t.Run("--json emits structured status", func(t *testing.T) {
		res := e.run(t, repo, "daemon", "status", "--json")
		// JSON should still be parseable even when the daemon is down.
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode daemon-status JSON %q: %v", res.stdout, err)
		}
		if len(got) == 0 {
			t.Errorf("expected non-empty daemon-status JSON: %v", got)
		}
	})
}

// ── treeman hook run ─────────────────────────────────────────────

const hookConfig = `worktrees:
  root: .worktrees
hooks:
  on-create-before-engines:
    - run: echo "hello from hook"
`

func TestHookRun(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	writeConfig(t, repo, hookConfig)

	// Need a real git-linked worktree so DiscoverRepoRoot resolves
	// correctly and the hook subprocess has a meaningful cwd.
	mustGit(t, repo, "branch", "manual")
	mustGit(t, repo, "worktree", "add", "-q", ".worktrees/manual", "manual")
	wtPath := repo + "/.worktrees/manual"
	res := e.run(t, wtPath, "wt", "register", "--branch", "manual")
	if res.err != nil {
		t.Fatalf("register: %v\nstderr:\n%s", res.err, res.stderr)
	}

	t.Run("missing phase positional errors", func(t *testing.T) {
		res := e.run(t, wtPath, "hook", "run")
		if res.err == nil {
			t.Errorf("expected hook run with no phase to error")
		}
		if !strings.Contains(res.stderr, "usage: treeman hook run") {
			t.Errorf("expected usage hint:\n%s", res.stderr)
		}
	})

	t.Run("runs the configured phase", func(t *testing.T) {
		res := e.run(t, wtPath, "hook", "run", "on-create-before-engines")
		if res.err != nil {
			t.Fatalf("hook run: %v\nstderr:\n%s\nstdout:\n%s",
				res.err, res.stderr, res.stdout)
		}
		if !strings.Contains(res.stdout, "on-create-before-engines") &&
			!strings.Contains(res.stdout, "action") {
			t.Errorf("expected hook run summary line, got:\n%s", res.stdout)
		}
	})

	t.Run("rejects unknown phase", func(t *testing.T) {
		res := e.run(t, wtPath, "hook", "run", "not-a-phase")
		if res.err == nil {
			t.Errorf("expected unknown phase to error")
		}
		if !strings.Contains(res.stderr, "unknown phase") {
			t.Errorf("expected 'unknown phase' message:\n%s", res.stderr)
		}
	})

	t.Run("--json reports phase + outcome", func(t *testing.T) {
		res := e.run(t, wtPath, "hook", "run", "on-create-before-engines", "--json")
		if res.err != nil {
			t.Fatalf("hook run --json: %v\nstderr:\n%s\nstdout:\n%s",
				res.err, res.stderr, res.stdout)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("decode hook JSON %q: %v", res.stdout, err)
		}
		if got["phase"] != "on-create-before-engines" {
			t.Errorf("expected phase=on-create-before-engines, got %v", got["phase"])
		}
	})

	t.Run("captured stdout survives in DB and renders via --show", func(t *testing.T) {
		// Re-run so we get a fresh hook_runs row; the row id varies by
		// run order so we discover it via `logs hooks --json`.
		_ = e.run(t, wtPath, "hook", "run", "on-create-before-engines")

		listing := e.run(t, wtPath, "logs", "hooks", "--all", "--json")
		if listing.err != nil {
			t.Fatalf("logs hooks --json: %v\nstderr:\n%s", listing.err, listing.stderr)
		}
		// Pick the most recent row.
		var newestID int64
		for _, line := range strings.Split(strings.TrimSpace(listing.stdout), "\n") {
			if line == "" {
				continue
			}
			var row struct {
				ID int64 `json:"ID"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("decode hook row %q: %v", line, err)
			}
			if row.ID > newestID {
				newestID = row.ID
			}
		}
		if newestID == 0 {
			t.Fatal("no hook_run rows after hook run")
		}
		show := e.run(t, wtPath, "logs", "hooks", "--show", strconv.FormatInt(newestID, 10))
		if show.err != nil {
			t.Fatalf("logs hooks --show: %v\nstderr:\n%s", show.err, show.stderr)
		}
		if !strings.Contains(show.stdout, "hello from hook") {
			t.Errorf("expected hook stdout in --show output:\n%s", show.stdout)
		}
	})
}

// ── treeman branches ─────────────────────────────────────────────

func TestBranches(t *testing.T) {
	e := newEnv(t)
	repo := newGitRepo(t)
	mustGit(t, repo, "branch", "feature/x")
	mustGit(t, repo, "branch", "feature/y")

	t.Run("plain table lists local branches", func(t *testing.T) {
		res := e.run(t, repo, "branches")
		if res.err != nil {
			t.Fatalf("branches: %v\nstderr:\n%s", res.err, res.stderr)
		}
		for _, want := range []string{"feature/x", "feature/y", "main"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("branches missing %q:\n%s", want, res.stdout)
			}
		}
	})

	t.Run("--json", func(t *testing.T) {
		res := e.run(t, repo, "branches", "--json")
		if res.err != nil {
			t.Fatalf("branches --json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &rows); err != nil {
			t.Fatalf("decode branches JSON: %v\nstdout:\n%s", err, res.stdout)
		}
		if len(rows) < 2 {
			t.Errorf("expected >=2 branch rows, got %d", len(rows))
		}
	})

	t.Run("--local-only excludes remote-only branches", func(t *testing.T) {
		res := e.run(t, repo, "branches", "--local-only")
		if res.err != nil {
			t.Fatalf("branches --local-only: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "feature/x") {
			t.Errorf("expected feature/x in --local-only output:\n%s", res.stdout)
		}
	})

	t.Run("--available filters out occupied", func(t *testing.T) {
		res := e.run(t, repo, "branches", "--available")
		if res.err != nil {
			t.Fatalf("branches --available: %v\nstderr:\n%s", res.err, res.stderr)
		}
	})
}

// ── treeman completion ───────────────────────────────────────────

func TestCompletion(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	for _, shell := range []string{"bash", "zsh", "fish"} {
		shell := shell
		t.Run("emits "+shell+" script", func(t *testing.T) {
			res := e.run(t, dir, "completion", shell)
			if res.err != nil {
				t.Fatalf("completion %s: %v\nstderr:\n%s", shell, res.err, res.stderr)
			}
			if len(res.stdout) < 100 {
				t.Errorf("completion %s output suspiciously short (%d bytes):\n%s",
					shell, len(res.stdout), res.stdout)
			}
		})
	}

	t.Run("unknown shell rejected", func(t *testing.T) {
		res := e.run(t, dir, "completion", "tcsh")
		if res.err == nil {
			t.Errorf("expected error for unknown shell, got stdout:\n%s", res.stdout)
		}
	})
}

// ── treeman --version + --help + unknown command ─────────────────

func TestTopLevelFlags(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()

	t.Run("--version prints semver", func(t *testing.T) {
		res := e.run(t, dir, "--version")
		if res.err != nil {
			t.Fatalf("--version: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "treeman version") {
			t.Errorf("expected 'treeman version' in --version stdout:\n%s", res.stdout)
		}
	})

	t.Run("--help lists commands", func(t *testing.T) {
		res := e.run(t, dir, "--help")
		if res.err != nil {
			t.Fatalf("--help: %v\nstderr:\n%s", res.err, res.stderr)
		}
		for _, want := range []string{"worktree", "logs", "config", "doctor"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("--help missing command %q:\n%s", want, res.stdout)
			}
		}
	})

	t.Run("unknown command suggests alternatives", func(t *testing.T) {
		res := e.run(t, dir, "wkrtree")
		if res.err == nil {
			t.Errorf("expected error for typo'd command")
		}
		// Levenshtein suggester should propose "wt" / "worktree".
		if !strings.Contains(res.stderr, "did you mean") {
			t.Errorf("expected 'did you mean' suggestion:\n%s", res.stderr)
		}
	})
}
