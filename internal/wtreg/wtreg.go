// Package wtreg holds worktree-registry mutation helpers shared by
// the `treeman registry` CLI commands and the MCP `registry_*`
// tools. The actual lifecycle (git worktree create/remove + hook
// dispatch) lives elsewhere; this package is just the SQLite-side
// reconciliation glue.
package wtreg

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// dbRow is a worktree row's identity + main-flag, as read during Repair.
type dbRow struct {
	id     int64
	isMain bool
}

// timeNow is exposed as a var so tests could swap a fixed clock if
// they ever need deterministic deleted_at timestamps. Currently
// production-only.
var timeNow = time.Now

// RepairResult is the per-action diff produced by Repair.
type RepairResult struct {
	Registered   []string `json:"registered"`
	Unregistered []string `json:"unregistered"`
	Errors       []string `json:"errors,omitempty"`
}

// Repair reconciles the SQLite registry with `git worktree list` for
// `repoRoot`: registers paths git knows about that SQLite doesn't,
// and marks deleted paths SQLite knows that git doesn't. Pure
// SQLite-side — does not touch git itself.
//
// All writes happen inside one SQL transaction so a mid-flight
// failure leaves the registry untouched instead of half-repaired.
// On any per-row error we collect it into `out.Errors` and continue;
// the transaction commits only if *every* row succeeded so the user
// sees a consistent snapshot or none at all. On commit failure the
// rollback re-establishes the pre-call state.
//
// detectBranch is injected so callers can plug their own .git/HEAD
// reader (the cmd package owns the canonical implementation; mcp
// imports a copy that ignores gitlinks). Passing nil falls back to
// emitting an empty branch.
func Repair(ctx context.Context, st *store.Store, repoRoot string, detectBranch func(path string) string) (RepairResult, error) {
	if detectBranch == nil {
		detectBranch = func(string) string { return "" }
	}
	gitPaths, err := GitWorktreePaths(ctx, repoRoot)
	if err != nil {
		return RepairResult{}, err
	}
	// EnsureRepo runs outside the tx — it's the lookup we need for
	// the subsequent queries and is idempotent.
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return RepairResult{}, fmt.Errorf("ensure repo: %w", err)
	}

	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return RepairResult{}, fmt.Errorf("begin tx: %w", err)
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback() // no-op when Commit already ran
		}
	}()

	dbWTs, err := loadRepairRows(ctx, tx, repoID)
	if err != nil {
		return RepairResult{}, err
	}

	out := RepairResult{}
	gitSet := map[string]bool{}
	// The repo root is the main worktree; `git worktree list` always
	// reports it. GitWorktreePaths deliberately omits it (it only
	// scans .git/worktrees/<name>/gitdir entries, which cover linked
	// worktrees), so add it explicitly when the directory still
	// exists. Without this, a repaired repo would always have its
	// main-wt row unregistered alongside genuinely dead rows.
	if fi, err := os.Stat(repoRoot); err == nil && fi.IsDir() {
		gitSet[repoRoot] = true
	}
	now := nowMillis()
	repairRegister(ctx, tx, &out, gitSet, gitPaths, dbWTs, repoRoot, repoID, now, detectBranch)
	repairUnregister(ctx, tx, &out, gitSet, dbWTs, now)

	if len(out.Errors) > 0 {
		// Any per-row failure poisons the whole reconcile — better
		// to surface the drift to the operator than to commit a
		// partial fix that's harder to reason about.
		rolledBack = true
		_ = tx.Rollback()
		return out, nil
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("commit tx: %w", err)
	}
	rolledBack = true
	return out, nil
}

// loadRepairRows reads the live (non-deleted) worktree rows for a repo
// into a path-keyed map. Extracted from Repair so the rows handle is
// scoped to a single defer-closed function.
func loadRepairRows(ctx context.Context, tx *sql.Tx, repoID int64) (map[string]dbRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, path, is_main FROM worktrees WHERE repo_id = ? AND deleted_at IS NULL`, repoID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	dbWTs := map[string]dbRow{}
	for rows.Next() {
		var id int64
		var p string
		var isMain int
		if err := rows.Scan(&id, &p, &isMain); err == nil {
			dbWTs[p] = dbRow{id: id, isMain: isMain == 1}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dbWTs, nil
}

// repairRegister registers every git-known path missing from SQLite.
// Mutates out (Registered / Errors) and seeds gitSet so the caller's
// subsequent unregister pass can tell live paths from dead ones.
// Extracted from Repair as a pure mechanical lift of the registration
// loop.
func repairRegister(
	ctx context.Context,
	tx *sql.Tx,
	out *RepairResult,
	gitSet map[string]bool,
	gitPaths []string,
	dbWTs map[string]dbRow,
	repoRoot string,
	repoID, now int64,
	detectBranch func(path string) string,
) {
	for _, p := range gitPaths {
		gitSet[p] = true
		if _, ok := dbWTs[p]; ok {
			continue
		}
		if p == repoRoot {
			// Main worktree rows are owned by `treeman main enable`;
			// skip the auto-register branch (which would synthesise a
			// path-hash slug). The row, if any, is already preserved
			// by the gitSet seed above.
			continue
		}
		branch := detectBranch(p)
		sl := slug.For(p, branch)
		// Mirror of EnsureWorktree's "SELECT then INSERT" — runs
		// against tx so this call participates in the transaction
		// instead of opening a fresh connection on st.DB.
		var existingID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM worktrees WHERE path = ?", p).Scan(&existingID)
		if err == nil {
			// Row already exists (likely marked deleted from a prior
			// run) — clear deleted_at + refresh metadata.
			if _, err := tx.ExecContext(ctx, `UPDATE worktrees
				SET slug = ?, branch = NULLIF(?,''), deleted_at = NULL, last_visited_at = ?
				WHERE id = ?`, sl.Value, branch, now, existingID); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("register %s: %v", p, err))
				continue
			}
		} else {
			var br any
			if branch != "" {
				br = branch
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO worktrees(repo_id, path, slug, branch, created_at, last_visited_at) VALUES (?, ?, ?, ?, ?, ?)",
				repoID, p, sl.Value, br, now, now); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("register %s: %v", p, err))
				continue
			}
		}
		out.Registered = append(out.Registered, p)
	}
}

// repairUnregister marks every SQLite-known path that git no longer
// reports as deleted. Mutates out (Unregistered / Errors). Extracted
// from Repair as a pure mechanical lift of the unregistration loop.
func repairUnregister(
	ctx context.Context,
	tx *sql.Tx,
	out *RepairResult,
	gitSet map[string]bool,
	dbWTs map[string]dbRow,
	now int64,
) {
	for p, row := range dbWTs {
		if gitSet[p] {
			continue
		}
		if row.isMain {
			// Defensive: an is_main row whose path is missing from
			// git's view is almost always our own gitSet gap, not a
			// dead row. Leave it alone — `treeman main disable` is the
			// supported way to retire a main-wt enrollment.
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE worktrees SET deleted_at = ? WHERE id = ?`, now, row.id); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("unregister %s: %v", p, err))
			continue
		}
		out.Unregistered = append(out.Unregistered, p)
	}
}

// nowMillis mirrors store.nowMillis (unexported there). Kept here as
// a tiny copy so wtreg doesn't need an exported store helper just
// for one timestamp.
func nowMillis() int64 { return timeNow().UnixMilli() }

// GitWorktreePaths returns absolute paths of linked worktrees (the
// MAIN repo itself is excluded). Reads `.git/worktrees/<name>/gitdir`
// directly instead of shelling out to `git worktree list --porcelain`
// — the file format is documented + stable, and skipping the
// subprocess saves ~30ms per call (matters for the daemon drift
// loop + doctor + `wt register` paths that all hit this).
//
// The `gitdir` file points at `<linked-wt>/.git`, so we trim the
// trailing `.git` element to get the worktree directory itself.
// Broken / pruned entries (where the worktree directory has been
// deleted from disk) are silently dropped.
func GitWorktreePaths(_ context.Context, repoRoot string) ([]string, error) {
	wtDir := filepath.Join(repoRoot, ".git", "worktrees")
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No linked worktrees registered with git yet — not an
			// error, just no rows.
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", wtDir, err)
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		gitdir := filepath.Join(wtDir, e.Name(), "gitdir")
		b, err := os.ReadFile(gitdir)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(string(b))
		// `gitdir` ends with `<linked-wt>/.git`. Strip the suffix to
		// recover the worktree's own root.
		p = strings.TrimSuffix(p, string(filepath.Separator)+".git")
		if p == "" || p == repoRoot {
			continue
		}
		// Verify the worktree still exists on disk so we don't return
		// stale entries the user removed without `git worktree prune`.
		if _, err := os.Stat(p); err != nil {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}
