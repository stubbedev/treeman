//go:build e2e

// Package logs_e2e exercises the `treeman logs {tail,grep,hooks,purge}`
// commands end-to-end against the real binary.
//
// The store is seeded with direct SQL inserts so timestamps, payloads,
// and worktree-row paths are deterministic. Each subtest invokes the
// compiled binary with a controlled cwd and a controlled `TREEMAN_DB_PATH`
// and asserts on stdout/stderr/exit-code.
package logs_e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/store"
)

type fixture struct {
	bin string

	dbPath  string
	homeDir string

	repo1Dir string
	repo2Dir string

	wt1aPath string // repo1, slug=wt1a, branch=feature/one
	wt1bPath string // repo1, slug=wt1b, branch=feature/two
	wt2aPath string // repo2, slug=wt2a, branch=hotfix

	repo1ID int64
	repo2ID int64

	wt1aID int64
	wt1bID int64
	wt2aID int64

	now time.Time
}

// setup builds the binary once, lays down two repo trees with three
// worktrees between them, and seeds events + hook_runs covering every
// dimension the CLI flags filter on.
func setup(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH (needed to build the test binary)")
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot = filepath.Dir(filepath.Dir(repoRoot)) // up to project root

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "treeman")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/treeman")
	build.Dir = repoRoot
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build treeman: %v", err)
	}

	// Lay down filesystem fixtures. Repos get a `.treeman.yaml` marker
	// so `DiscoverRepoRoot` finds them without needing `git init`.
	fxRoot := t.TempDir()
	homeDir := t.TempDir()

	repo1 := filepath.Join(fxRoot, "repo1")
	repo2 := filepath.Join(fxRoot, "repo2")
	wt1a := filepath.Join(repo1, ".worktrees", "feature_one")
	wt1b := filepath.Join(repo1, ".worktrees", "feature_two")
	wt2a := filepath.Join(repo2, ".worktrees", "hotfix")
	for _, d := range []string{repo1, repo2, wt1a, wt1b, wt2a} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []string{repo1, repo2} {
		if err := os.WriteFile(filepath.Join(r, ".treeman.yaml"), []byte("# fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Seed the store.
	dbPath := filepath.Join(t.TempDir(), "treeman.db")
	ctx := context.Background()
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	repo1ID, err := st.EnsureRepo(ctx, repo1, "repo1")
	if err != nil {
		t.Fatal(err)
	}
	repo2ID, err := st.EnsureRepo(ctx, repo2, "repo2")
	if err != nil {
		t.Fatal(err)
	}
	wt1aID, err := st.EnsureWorktree(ctx, repo1ID, wt1a, "wt1a", "feature/one")
	if err != nil {
		t.Fatal(err)
	}
	wt1bID, err := st.EnsureWorktree(ctx, repo1ID, wt1b, "wt1b", "feature/two")
	if err != nil {
		t.Fatal(err)
	}
	wt2aID, err := st.EnsureWorktree(ctx, repo2ID, wt2a, "wt2a", "hotfix")
	if err != nil {
		t.Fatal(err)
	}

	// Deterministic timeline anchored 1h ago so `--since 10m` excludes
	// older events while `--since 2h` keeps them.
	now := time.Now()
	mins := func(m int) int64 { return now.Add(-time.Duration(m) * time.Minute).UnixMilli() }

	type seedEv struct {
		ts        int64
		level     string
		repo      int64
		wt        int64
		eventType string
		phase     string
		message   string
		payload   string
		durMs     int64
	}
	events := []seedEv{
		{mins(120), "debug", repo1ID, wt1aID, "prepare:start", "precreate", "wt1a old debug", `{"engine":"mysql"}`, 0},
		{mins(90), "info", repo1ID, wt1aID, "prepare:end", "postcreate", "wt1a info hit alpha", `{"engine":"mysql","cache":"hit"}`, 1200},
		{mins(45), "warn", repo1ID, wt1aID, "clones:end", "postcreate", "wt1a warn fanout slow", `{"slowest_ms":7000}`, 7000},
		{mins(5), "error", repo1ID, wt1aID, "worktree:create:end", "postcreate", "wt1a recent error needle", `{"err":"boom"}`, 0},

		{mins(80), "info", repo1ID, wt1bID, "prepare:start", "precreate", "wt1b info bravo", `{"engine":"postgres"}`, 0},
		{
			mins(20),
			"info",
			repo1ID,
			wt1bID,
			"prepare:end",
			"postcreate",
			"wt1b info bravo done",
			`{"engine":"postgres","cache":"miss"}`,
			800,
		},

		{mins(70), "warn", repo2ID, wt2aID, "snapshot_create", "precreate", "wt2a snapshot warn", `{"size":12345}`, 4500},
		{mins(15), "info", repo2ID, wt2aID, "worktree:create:end", "postcreate", "wt2a finalize done", `{"final":true}`, 0},
	}
	for _, e := range events {
		var dur any
		if e.durMs > 0 {
			dur = e.durMs
		}
		if _, err := st.DB.ExecContext(ctx,
			`INSERT INTO events(ts, level, repo_id, worktree_id, event_type, phase, message, payload_json, duration_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ts, e.level, e.repo, e.wt, e.eventType, e.phase, e.message, e.payload, dur); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}

	type seedHook struct {
		wt       int64
		phase    string
		groupIdx int
		command  string
		started  int64
		finished int64
		exit     int
	}
	hooks := []seedHook{
		{wt1aID, "create-before-engines", 0, "echo a", mins(100), mins(100) + 250, 0},
		{wt1aID, "create-after-engines", 1, "echo b", mins(60), mins(60) + 500, 0},
		{wt1bID, "create-before-engines", 0, "echo c", mins(40), mins(40) + 100, 0},
		{wt2aID, "create-before-engines", 0, "echo d", mins(10), mins(10) + 80, 1},
	}
	var hookRunIDs []int64
	for _, h := range hooks {
		res, err := st.DB.ExecContext(ctx,
			`INSERT INTO hook_runs(worktree_id, phase, group_idx, command, started_at, finished_at, exit_code, stdout_tail, stderr_tail)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '', '')`,
			h.wt, h.phase, h.groupIdx, h.command, h.started, h.finished, h.exit)
		if err != nil {
			t.Fatalf("seed hook_run: %v", err)
		}
		id, _ := res.LastInsertId()
		hookRunIDs = append(hookRunIDs, id)
	}
	// Attach a captured-output chunk to the first hook so the
	// `logs hooks --show` rendering path has bytes to stream.
	// ANSI escape kept intact so the test pins the round-trip.
	chunkBody := []byte("\x1b[32mhello\x1b[0m from hook stdout\n")
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO hook_log_chunks(hook_run_id, ts, stream, body)
		 VALUES (?, ?, 'merged', ?)`,
		hookRunIDs[0], time.Now().UnixMilli(), chunkBody); err != nil {
		t.Fatalf("seed hook_log_chunk: %v", err)
	}

	return &fixture{
		bin:      binPath,
		dbPath:   dbPath,
		homeDir:  homeDir,
		repo1Dir: repo1,
		repo2Dir: repo2,
		wt1aPath: wt1a,
		wt1bPath: wt1b,
		wt2aPath: wt2a,
		repo1ID:  repo1ID,
		repo2ID:  repo2ID,
		wt1aID:   wt1aID,
		wt1bID:   wt1bID,
		wt2aID:   wt2aID,
		now:      now,
	}
}

type cliResult struct {
	stdout string
	stderr string
	err    error
}

func runCLI(t *testing.T, fx *fixture, cwd string, args ...string) cliResult {
	t.Helper()
	cmd := exec.Command(fx.bin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"TREEMAN_DB_PATH="+fx.dbPath,
		"HOME="+fx.homeDir,
		"XDG_DATA_HOME="+fx.homeDir,
		"XDG_STATE_HOME="+fx.homeDir,
		"XDG_CONFIG_HOME="+fx.homeDir,
		"TREEMAN_NO_PAGER=1",
		"NO_COLOR=1",
	)
	var sout, serr strings.Builder
	cmd.Stdout = &sout
	cmd.Stderr = &serr
	err := cmd.Run()
	return cliResult{stdout: sout.String(), stderr: serr.String(), err: err}
}

func TestLogsCLI(t *testing.T) {
	fx := setup(t)

	// /tmp is used as an "outside any repo" cwd. We pick t.TempDir()
	// instead of the real /tmp so HOME isolation actually holds.
	outsideDir := t.TempDir()

	cases := []struct {
		name        string
		cwd         string
		args        []string
		wantStdout  []string // every substring must appear
		notStdout   []string // none of these may appear
		wantStderr  []string
		notStderr   []string
		wantExitErr bool
	}{
		// ── tail: scope resolution ────────────────────────────────
		{
			name:       "tail/cwd-inside-worktree auto-resolves",
			cwd:        fx.wt1aPath,
			args:       []string{"logs", "tail", "-n", "50"},
			wantStdout: []string{"wt1a recent error needle", "wt1a warn fanout slow"},
			notStdout:  []string{"wt1b info bravo", "wt2a snapshot warn"},
			wantStderr: []string{"# scope: worktree=wt1a"},
		},
		{
			name:       "tail/--all from inside a worktree returns every event",
			cwd:        fx.wt1aPath,
			args:       []string{"logs", "tail", "-n", "50", "--all"},
			wantStdout: []string{"wt1a recent error needle", "wt1b info bravo", "wt2a snapshot warn"},
			notStderr:  []string{"# scope: worktree="},
		},
		{
			name:       "tail/--all from outside any repo returns every event",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all"},
			wantStdout: []string{"wt1a recent error needle", "wt1b info bravo", "wt2a snapshot warn"},
		},
		{
			name:       "tail/cwd at repo root narrows to that repo only",
			cwd:        fx.repo1Dir,
			args:       []string{"logs", "tail", "-n", "50"},
			wantStdout: []string{"wt1a recent error needle", "wt1b info bravo"},
			notStdout:  []string{"wt2a snapshot warn"},
		},
		{
			name:       "tail/--repo overrides cwd discovery",
			cwd:        fx.wt1aPath,
			args:       []string{"logs", "tail", "-n", "50", "--repo", fx.repo2Dir},
			wantStdout: []string{"wt2a snapshot warn", "wt2a finalize done"},
			notStdout:  []string{"wt1a recent error needle"},
		},

		// ── tail: --worktree resolves by slug/branch/basename ─────
		{
			name:       "tail/--worktree by slug",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--worktree", "wt1b"},
			wantStdout: []string{"wt1b info bravo"},
			notStdout:  []string{"wt1a recent error needle", "wt2a snapshot warn"},
		},
		{
			name:       "tail/--worktree by branch name",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--worktree", "feature/one"},
			wantStdout: []string{"wt1a recent error needle"},
			notStdout:  []string{"wt1b info bravo"},
		},
		{
			name:       "tail/--worktree by path basename",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--worktree", "hotfix"},
			wantStdout: []string{"wt2a snapshot warn"},
			notStdout:  []string{"wt1a recent error needle"},
		},
		{
			name:        "tail/--worktree unknown name errors",
			cwd:         outsideDir,
			args:        []string{"logs", "tail", "--worktree", "does-not-exist"},
			wantStderr:  []string{`no worktree matches "does-not-exist"`},
			wantExitErr: true,
		},

		// ── tail: column filters ─────────────────────────────────
		{
			name:       "tail/--level repeats and accumulates",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all", "--level", "warn", "--level", "error"},
			wantStdout: []string{"wt1a warn fanout slow", "wt1a recent error needle", "wt2a snapshot warn"},
			notStdout:  []string{"wt1a info hit alpha", "wt1b info bravo"},
		},
		{
			name:       "tail/invalid --level is dropped (validateLevels), valid one survives",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all", "--level", "bogus", "--level", "error"},
			wantStdout: []string{"wt1a recent error needle"},
			notStdout:  []string{"wt1a info hit alpha"},
		},
		{
			name: "tail/--event-type repeats",
			cwd:  outsideDir,
			args: []string{
				"logs",
				"tail",
				"-n",
				"50",
				"--all",
				"--event-type",
				"snapshot_create",
				"--event-type",
				"worktree:create:end",
			},
			wantStdout: []string{"wt1a recent error needle", "wt2a snapshot warn", "wt2a finalize done"},
			notStdout:  []string{"wt1a info hit alpha", "wt1b info bravo"},
		},
		{
			name:       "tail/--phase filters",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all", "--phase", "precreate"},
			wantStdout: []string{"wt1a old debug", "wt1b info bravo", "wt2a snapshot warn"},
			notStdout:  []string{"wt1a recent error needle"},
		},
		{
			name:       "tail/--payload substring filters",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all", "--payload", `"engine":"postgres"`},
			wantStdout: []string{"wt1b info bravo"},
			notStdout:  []string{"wt1a info hit alpha"},
		},

		// ── tail: --since accepts duration + absolute ─────────────
		{
			name:       "tail/--since duration keeps recent events only",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "50", "--all", "--since", "30m"},
			wantStdout: []string{"wt1a recent error needle", "wt1b info bravo done", "wt2a finalize done"},
			notStdout:  []string{"wt1a old debug", "wt1a info hit alpha"},
		},
		{
			name:        "tail/--since bogus value errors",
			cwd:         outsideDir,
			args:        []string{"logs", "tail", "--all", "--since", "yesterday-ish"},
			wantStderr:  []string{"unrecognised --since value"},
			wantExitErr: true,
		},

		// ── tail: presentation flags ─────────────────────────────
		{
			name:       "tail/-n caps row count",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "2", "--all"},
			wantStdout: []string{"wt1a recent error needle"},
		},
		{
			name: "tail/--json emits NDJSON without scope preamble",
			cwd:  fx.wt1aPath,
			args: []string{"logs", "tail", "-n", "50", "--json"},
			// Field names are PascalCase because store.Event has no json
			// tags — the test pins that contract.
			wantStdout: []string{`"EventType"`, `"Level"`, `"WorktreeSlug":"wt1a"`},
			notStderr:  []string{"# scope:"},
		},
		{
			name:       "tail/--no-pager is accepted without error",
			cwd:        outsideDir,
			args:       []string{"logs", "tail", "-n", "3", "--all", "--no-pager"},
			wantStdout: []string{"wt1a recent error needle"},
		},

		// ── grep ─────────────────────────────────────────────────
		{
			name:        "grep/missing pattern errors",
			cwd:         outsideDir,
			args:        []string{"logs", "grep", "--all"},
			wantStderr:  []string{"usage: treeman logs grep <pattern>"},
			wantExitErr: true,
		},
		{
			name:       "grep/substring on message",
			cwd:        outsideDir,
			args:       []string{"logs", "grep", "needle", "--all"},
			wantStdout: []string{"wt1a recent error needle"},
			notStdout:  []string{"wt1b info bravo"},
		},
		{
			name:       "grep/--regex anchored",
			cwd:        outsideDir,
			args:       []string{"logs", "grep", "^wt2a snapshot", "--all", "--regex"},
			wantStdout: []string{"wt2a snapshot warn"},
			notStdout:  []string{"wt1a"},
		},
		{
			name:        "grep/--regex invalid pattern errors",
			cwd:         outsideDir,
			args:        []string{"logs", "grep", "[", "--all", "--regex"},
			wantStderr:  []string{"invalid regex"},
			wantExitErr: true,
		},
		{
			name:       "grep/--search-payload hits payload_json column",
			cwd:        outsideDir,
			args:       []string{"logs", "grep", `"cache":"miss"`, "--all", "--search-payload"},
			wantStdout: []string{"wt1b info bravo done"},
			notStdout:  []string{"wt1a info hit alpha"},
		},
		{
			name:       "grep/no-match emits 'no events matched' info line",
			cwd:        outsideDir,
			args:       []string{"logs", "grep", "zzzz_no_such_string", "--all"},
			wantStdout: []string{`no events matched "zzzz_no_such_string"`},
		},

		// ── hooks ────────────────────────────────────────────────
		{
			name:       "hooks/cwd auto-resolves to enclosing worktree",
			cwd:        fx.wt1aPath,
			args:       []string{"logs", "hooks", "-n", "10"},
			wantStdout: []string{"create-before-engines", "create-after-engines"},
			notStdout:  []string{"echo c", "echo d"},
			wantStderr: []string{"# scope: worktree=wt1a"},
		},
		{
			name:       "hooks/explicit worktree name",
			cwd:        outsideDir,
			args:       []string{"logs", "hooks", "wt1b"},
			wantStdout: []string{"echo c"},
			notStdout:  []string{"echo a", "echo d"},
		},
		{
			name:        "hooks/outside any repo without --all errors",
			cwd:         outsideDir,
			args:        []string{"logs", "hooks"},
			wantStderr:  []string{"cwd is not inside a registered worktree"},
			wantExitErr: true,
		},
		{
			name:       "hooks/--all spans every worktree and adds WORKTREE column",
			cwd:        outsideDir,
			args:       []string{"logs", "hooks", "-n", "20", "--all"},
			wantStdout: []string{"WORKTREE", "wt1a", "wt1b", "wt2a", "echo a", "echo c", "echo d"},
		},
		{
			name:       "hooks/--all --json emits NDJSON hook rows",
			cwd:        outsideDir,
			args:       []string{"logs", "hooks", "-n", "20", "--all", "--json"},
			wantStdout: []string{`"WorktreeSlug":"wt1a"`, `"WorktreeSlug":"wt2a"`},
		},
		{
			name:        "hooks/unknown worktree name errors",
			cwd:         outsideDir,
			args:        []string{"logs", "hooks", "nope"},
			wantStderr:  []string{`no worktree matches "nope"`},
			wantExitErr: true,
		},

		// ── hooks --show <id> renders the captured chunk ─────────
		{
			name:       "hooks/--show renders captured chunk verbatim (ANSI preserved)",
			cwd:        outsideDir,
			args:       []string{"logs", "hooks", "--all", "--show", "1"},
			wantStdout: []string{"\x1b[32mhello\x1b[0m from hook stdout"},
		},
		{
			name:        "hooks/--show on unknown id errors",
			cwd:         outsideDir,
			args:        []string{"logs", "hooks", "--show", "99999"},
			wantStderr:  []string{"no captured log for hook_run id=99999"},
			wantExitErr: true,
		},
		{
			name:       "hooks/--show --json emits chunk envelopes",
			cwd:        outsideDir,
			args:       []string{"logs", "hooks", "--all", "--show", "1", "--json"},
			wantStdout: []string{`"Stream":"merged"`, `"HookRunID":1`},
		},

		// ── purge ────────────────────────────────────────────────
		{
			name:        "purge/refuses unfiltered call",
			cwd:         outsideDir,
			args:        []string{"logs", "purge"},
			wantStderr:  []string{"at least one filter"},
			wantExitErr: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			res := runCLI(t, fx, tc.cwd, tc.args...)
			if tc.wantExitErr {
				if res.err == nil {
					t.Errorf("expected non-zero exit, got success\nstdout:\n%s\nstderr:\n%s", res.stdout, res.stderr)
				}
			} else if res.err != nil {
				t.Errorf("unexpected error: %v\nstdout:\n%s\nstderr:\n%s", res.err, res.stdout, res.stderr)
			}
			for _, s := range tc.wantStdout {
				if !strings.Contains(res.stdout, s) {
					t.Errorf("stdout missing %q\nstdout:\n%s", s, res.stdout)
				}
			}
			for _, s := range tc.notStdout {
				if strings.Contains(res.stdout, s) {
					t.Errorf("stdout unexpectedly contains %q\nstdout:\n%s", s, res.stdout)
				}
			}
			for _, s := range tc.wantStderr {
				if !strings.Contains(res.stderr, s) {
					t.Errorf("stderr missing %q\nstderr:\n%s", s, res.stderr)
				}
			}
			for _, s := range tc.notStderr {
				if strings.Contains(res.stderr, s) {
					t.Errorf("stderr unexpectedly contains %q\nstderr:\n%s", s, res.stderr)
				}
			}
		})
	}

	// ── purge actually deletes rows. Runs last so it doesn't break
	// earlier subtests' assertions on row presence. ────────────────
	t.Run("purge/by --level removes matching rows and reports count", func(t *testing.T) {
		res := runCLI(t, fx, outsideDir, "logs", "purge", "--level", "debug")
		if res.err != nil {
			t.Fatalf("purge: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout+res.stderr, "removed 1 event") {
			t.Errorf("expected 'removed 1 event' (1 debug row seeded), got\nstdout:\n%s\nstderr:\n%s",
				res.stdout, res.stderr)
		}
		// Confirm the row is gone.
		res = runCLI(t, fx, outsideDir, "logs", "tail", "--all", "--level", "debug", "-n", "50")
		if strings.Contains(res.stdout, "wt1a old debug") {
			t.Errorf("debug row should be purged, still present:\n%s", res.stdout)
		}
	})

	t.Run("purge/--older-than removes ancient rows", func(t *testing.T) {
		// At this point the only `info_hit_alpha`-aged row (90m) is the
		// oldest remaining; --older-than 75m should remove it plus any
		// other >75m rows.
		res := runCLI(t, fx, outsideDir, "logs", "purge", "--older-than", "75m")
		if res.err != nil {
			t.Fatalf("purge older-than: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// Confirm the 90m alpha event is gone but the 45m warn survives.
		res = runCLI(t, fx, outsideDir, "logs", "tail", "--all", "-n", "50")
		if strings.Contains(res.stdout, "wt1a info hit alpha") {
			t.Errorf("90m alpha event should be purged, still present:\n%s", res.stdout)
		}
		if !strings.Contains(res.stdout, "wt1a warn fanout slow") {
			t.Errorf("45m warn event should NOT be purged, missing:\n%s", res.stdout)
		}
	})

	t.Run("purge/--json reports rows_removed", func(t *testing.T) {
		res := runCLI(t, fx, outsideDir, "logs", "purge", "--worktree", "wt2a", "--json")
		if res.err != nil {
			t.Fatalf("purge json: %v\nstderr:\n%s", res.err, res.stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.stdout)), &got); err != nil {
			t.Fatalf("expected JSON, got %q: %v", res.stdout, err)
		}
		if _, ok := got["rows_removed"]; !ok {
			t.Errorf("missing rows_removed key: %v", got)
		}
	})
}

// TestLogsTailFollow verifies --follow streams new rows as they're
// inserted. Kept as its own test because it spawns a long-running
// process the table-driven body can't accommodate.
func TestLogsTailFollow(t *testing.T) {
	fx := setup(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, fx.bin, "logs", "tail", "--all", "--follow", "--json", "-n", "0")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(),
		"TREEMAN_DB_PATH="+fx.dbPath,
		"HOME="+fx.homeDir,
		"XDG_DATA_HOME="+fx.homeDir,
		"XDG_STATE_HOME="+fx.homeDir,
		"XDG_CONFIG_HOME="+fx.homeDir,
		"TREEMAN_NO_PAGER=1",
		"NO_COLOR=1",
	)
	var sout strings.Builder
	cmd.Stdout = &sout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start follow: %v", err)
	}

	// Give the follower a moment to drain the initial snapshot, then
	// insert a fresh event the loop should pick up within 500ms.
	time.Sleep(750 * time.Millisecond)

	st, err := store.Open(context.Background(), fx.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	needle := "follow_streamed_needle"
	if _, err := st.DB.ExecContext(context.Background(),
		`INSERT INTO events(ts, level, repo_id, worktree_id, event_type, message, payload_json)
		 VALUES (?, 'info', ?, ?, 'follow_test', ?, '{}')`,
		time.Now().UnixMilli(), fx.repo1ID, fx.wt1aID, needle); err != nil {
		t.Fatalf("insert follow row: %v", err)
	}

	// Wait for the follower to print the row, then terminate.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sout.String(), needle) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	_ = cmd.Wait()

	if !strings.Contains(sout.String(), needle) {
		t.Errorf("--follow never printed the newly-inserted row\nstdout:\n%s", sout.String())
	}
}
