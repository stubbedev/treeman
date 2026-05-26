//go:build e2e

// Package mainworktree_e2e drives the main-worktree feature
// end-to-end: opt the repo root into the branch-aware lifecycle, fire
// real HEAD + FS events, and assert the on-checkout / on-file-change
// hooks fire with the right env vars under three overlay variants
// (none / partial / full). Catches the dispatcher-slug + missing-
// overlay-in-rendering bugs that the pre-existing unit tests for the
// feature didn't cover.
//
// No docker is required: hooks fire in goroutines parallel to
// prepare.Run, so a missing database connection just logs a warning
// and never blocks the witness. The build tag is `e2e` so these stay
// out of the default `go test ./...` flow alongside the rest of the
// e2e suite.
package mainworktree_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/daemon"
	"github.com/stubbedev/treeman/internal/store"
)

// TestMainWorktreeOnCheckout opts the repo root into main-wt, starts
// the daemon's HEAD watcher, switches branches, and asserts the
// on-checkout hook fires with TREEMAN_SLUG=main_<branch>. The pre-
// fix dispatcher passed slug.For(repoRoot, branch) (a path-hash slug)
// which would write a wrong value to the witness AND overwrite the
// main row's slug column.
func TestMainWorktreeOnCheckout(t *testing.T) {
	requireGit(t)

	repoRoot := t.TempDir()
	witnessDir := filepath.Join(repoRoot, "touch")
	if err := os.MkdirAll(witnessDir, 0o755); err != nil {
		t.Fatal(err)
	}

	mustGit(t, "", "init", "-q", "-b", "main", repoRoot)
	mustGit(t, repoRoot, "config", "user.email", "t@t")
	mustGit(t, repoRoot, "config", "user.name", "t")
	writeFile(t, repoRoot, "README", "hi")
	yaml := `
main_worktree:
  enabled: true
hooks:
  on-checkout:
    - run: echo "$TREEMAN_SLUG" > ` + witnessDir + `/slug
`
	writeFile(t, repoRoot, ".treeman.yaml", yaml)
	mustGit(t, repoRoot, "add", "-A")
	mustGit(t, repoRoot, "commit", "-q", "-m", "init")
	mustGit(t, repoRoot, "checkout", "-q", "-b", "feature/x")
	writeFile(t, repoRoot, "README", "feature")
	mustGit(t, repoRoot, "commit", "-q", "-am", "feat")
	mustGit(t, repoRoot, "checkout", "-q", "main")

	st, state := bootDaemon(t, repoRoot)

	if _, err := daemon.EnrollMainWorktree(context.Background(), state, repoRoot); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := daemon.ResumeWorktreeWatcher(context.Background(), state, repoRoot, repoRoot); err != nil {
		t.Fatalf("resume wt watcher: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // let HEAD watcher seed

	mustGit(t, repoRoot, "checkout", "-q", "feature/x")

	witness := filepath.Join(witnessDir, "slug")
	body := waitForFile(t, witness, 15*time.Second)
	got := strings.TrimSpace(body)
	if got != "main_feature_x" {
		t.Fatalf("TREEMAN_SLUG witness = %q, want main_feature_x (the dispatcher leaked a path-hash slug)", got)
	}

	// Row's slug column must reflect the post-checkout branch — and
	// must stay main_<branch>, not get clobbered with a path-hash
	// slug by EnsureWorktree in fireTriggerActions.
	assertMainRow(t, st, repoRoot, "main_feature_x", "feature/x")
}

// TestMainWorktreeOnFileChange runs the on-file-change variant matrix
// (no overlay / partial overlay / full overlay) and asserts the hook
// fires with TREEMAN_WATCH_DB_NAME rendered from the right template.
// The pre-fix fireOnFileChange read cfg.Databases[i].NameTemplate
// without applying the main-wt overlay, so an overlay's name_template
// silently never reached the hook env.
func TestMainWorktreeOnFileChange(t *testing.T) {
	requireGit(t)

	cases := []struct {
		name          string
		overlayBlock  string // YAML under `main_worktree:` (after `enabled: true`)
		wantDBPrefix  string // TREEMAN_WATCH_DB_NAME prefix the witness must start with
		wantSlugInDB  string // The main row's slug column post-event
		branch        string // git branch at which the event fires
	}{
		{
			name:         "NoOverlay",
			overlayBlock: "",
			wantDBPrefix: "tm_mw_main_",
			wantSlugInDB: "main_main",
			branch:       "main",
		},
		{
			name: "PartialOverlay_NameTemplateOnly",
			overlayBlock: `
  databases:
    - name_template: "tm_mw_dev_{slug}"
`,
			wantDBPrefix: "tm_mw_dev_main_",
			wantSlugInDB: "main_main",
			branch:       "main",
		},
		{
			name: "FullOverlay_NameTemplateAndFanout",
			overlayBlock: `
  databases:
    - name_template: "tm_mw_full_{slug}"
      fanout: 0
      test_clones:
        clones: 0
        name_template: ""
`,
			wantDBPrefix: "tm_mw_full_main_",
			wantSlugInDB: "main_main",
			branch:       "main",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			witnessDir := filepath.Join(repoRoot, "touch")
			if err := os.MkdirAll(witnessDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repoRoot, "db/migrations"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeFile(t, repoRoot, "db/migrations/000_init.sql", "CREATE TABLE x(id INT);")

			mustGit(t, "", "init", "-q", "-b", tc.branch, repoRoot)
			mustGit(t, repoRoot, "config", "user.email", "t@t")
			mustGit(t, repoRoot, "config", "user.name", "t")

			yaml := `
main_worktree:
  enabled: true` + tc.overlayBlock + `
debounce_ms: 100
databases:
  - engine: mysql
    name_template: "tm_mw_{slug}"
    test_clones:
      clones: 4
      name_template: "tm_mw_{slug}_t{n}"
    fanout: 4
    inputs:
      - { glob: "db/migrations/*.sql", label: migrations, hash: filename }
hooks:
  on-file-change:
    - match: migrations
      run: 'echo "$TREEMAN_SLUG|$TREEMAN_WATCH_DB_NAME|$TREEMAN_WATCH_LABEL|$TREEMAN_WATCH_ENGINE" > ` + witnessDir + `/event'
`
			writeFile(t, repoRoot, ".treeman.yaml", yaml)
			mustGit(t, repoRoot, "add", "-A")
			mustGit(t, repoRoot, "commit", "-q", "-m", "init")

			st, state := bootDaemon(t, repoRoot)

			if _, err := daemon.EnrollMainWorktree(context.Background(), state, repoRoot); err != nil {
				t.Fatalf("enroll: %v", err)
			}
			if err := daemon.ResumeWorktreeWatcher(context.Background(), state, repoRoot, repoRoot); err != nil {
				t.Fatalf("resume wt watcher: %v", err)
			}
			time.Sleep(500 * time.Millisecond) // fsnotify subscribe + HEAD seed

			writeFile(t, repoRoot, "db/migrations/001_new.sql", "ALTER TABLE x ADD COLUMN n INT;")

			witness := filepath.Join(witnessDir, "event")
			body := waitForFile(t, witness, 15*time.Second)
			parts := strings.Split(strings.TrimSpace(body), "|")
			if len(parts) != 4 {
				t.Fatalf("witness body %q: want 4 |-separated fields, got %d", body, len(parts))
			}
			gotSlug, gotDBName, gotLabel, gotEngine := parts[0], parts[1], parts[2], parts[3]

			if gotSlug != "main_main" {
				t.Errorf("TREEMAN_SLUG = %q, want main_main (dispatcher used wrong slug for main-wt)", gotSlug)
			}
			if !strings.HasPrefix(gotDBName, tc.wantDBPrefix) {
				t.Errorf("TREEMAN_WATCH_DB_NAME = %q, want prefix %q (overlay did not flow into the rendered template)",
					gotDBName, tc.wantDBPrefix)
			}
			if gotLabel != "migrations" {
				t.Errorf("TREEMAN_WATCH_LABEL = %q, want migrations", gotLabel)
			}
			if gotEngine != "mysql" {
				t.Errorf("TREEMAN_WATCH_ENGINE = %q, want mysql", gotEngine)
			}

			assertMainRow(t, st, repoRoot, tc.wantSlugInDB, tc.branch)
		})
	}
}

// bootDaemon opens a fresh sqlite store + daemon state rooted at the
// repo. Returns the raw store and the live daemon.State so
// subsequent assertions can query rows directly.
func bootDaemon(t *testing.T, repoRoot string) (*store.Store, *daemon.State) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tm.db")
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	state := daemon.NewState(ctx, st)
	return st, state
}

// assertMainRow queries the live store for the repo's main-wt row and
// reports a clear failure when the slug column doesn't match. After
// the fix, dispatchers route through resolveWorktreeIdentity →
// EnsureMainWorktree, so the slug stays branch-aware; pre-fix, the
// EnsureWorktree call in the dispatcher would have stomped it to a
// path-hash value.
func assertMainRow(t *testing.T, st *store.Store, repoRoot, wantSlug, wantBranch string) {
	t.Helper()
	ctx := context.Background()
	repoID, err := st.LookupRepoID(ctx, repoRoot)
	if err != nil || repoID == 0 {
		t.Fatalf("lookup repo id: %v (id=%d)", err, repoID)
	}
	// Give the dispatcher a brief moment to finish writing — the
	// witness arrives via a separate goroutine and the EnsureMain
	// Worktree call inside the dispatcher may not have committed yet.
	deadline := time.Now().Add(2 * time.Second)
	var row store.WorktreeRow
	for time.Now().Before(deadline) {
		row, err = st.LookupMainWorktree(ctx, repoID)
		if err == nil && row.ID != 0 && row.Slug == wantSlug && row.Branch == wantBranch {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("LookupMainWorktree: %v", err)
	}
	if row.ID == 0 || !row.IsMain {
		t.Fatalf("main row missing or not is_main: %+v", row)
	}
	if row.Slug != wantSlug {
		t.Errorf("main row slug = %q, want %q (a dispatcher EnsureWorktree call overwrote it)",
			row.Slug, wantSlug)
	}
	if row.Branch != wantBranch {
		t.Errorf("main row branch = %q, want %q", row.Branch, wantBranch)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil && len(body) > 0 {
			return string(body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared (or stayed empty) within %s", path, timeout)
	return ""
}

