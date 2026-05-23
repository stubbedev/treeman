package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/hooks"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
)

// LifecycleWatcher tails the git-administrative directory
// `<common-dir>/worktrees/` of one repo and fires hook phases when
// worktrees appear/disappear outside of the treeman CLI.
//
// Always on: the daemon spawns a LifecycleWatcher for every
// registered repo unconditionally. Worktree add/remove is
// infrastructure (not user policy), so there's no opt-in toggle.
type LifecycleWatcher struct {
	repoID    int64
	repoPath  string
	adminRoot string

	fsw *fsnotify.Watcher
	st  *State

	mu       sync.Mutex
	seen     map[string]string // adminDir → workingTreePath
	debounce time.Duration

	pendingMu     sync.Mutex
	pendingCreate map[string]*time.Timer
}

// StartLifecycleWatcher subscribes to a repo's `<common-dir>/worktrees/`
// directory and runs an initial reconcile pass against the DB. Idempotent
// per repoPath via the daemon state's watcher registry: a second call
// cancels the prior watcher first.
func StartLifecycleWatcher(ctx context.Context, st *State, repoID int64, repoPath string) (*LifecycleWatcher, error) {
	if repoPath == "" {
		return nil, fmt.Errorf("lifecycle: empty repo_path")
	}
	commonDir, err := resolveGitCommonDir(repoPath)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: resolve common-dir: %w", err)
	}
	adminRoot := filepath.Join(commonDir, "worktrees")
	if err := os.MkdirAll(adminRoot, 0o755); err != nil {
		return nil, fmt.Errorf("lifecycle: mkdir %s: %w", adminRoot, err)
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("lifecycle: fsnotify new: %w", err)
	}
	if err := fsw.Add(adminRoot); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("lifecycle: add %s: %w", adminRoot, err)
	}

	lw := &LifecycleWatcher{
		repoID:        repoID,
		repoPath:      repoPath,
		adminRoot:     adminRoot,
		fsw:           fsw,
		st:            st,
		seen:          map[string]string{},
		debounce:      750 * time.Millisecond,
		pendingCreate: map[string]*time.Timer{},
	}

	if err := lw.reconcile(ctx); err != nil {
		slog.Warn("lifecycle reconcile failed", "repo", repoPath, "err", err)
	}

	wctx, cancel := context.WithCancel(st.BgCtx)
	entry := &WatcherEntry{
		RepoPath: repoPath,
		Cancel: func() {
			cancel()
			_ = fsw.Close()
		},
	}
	st.RegisterLifecycleWatcher(repoPath, entry)
	safeGo("lifecycle:"+repoPath, func() { lw.loop(wctx) })
	slog.Info("lifecycle watcher started", "repo", repoPath, "admin_root", adminRoot)
	return lw, nil
}

// loop drains the fsnotify channel, filtering to direct subdirs of
// the administrative root.
func (lw *LifecycleWatcher) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-lw.fsw.Events:
			if !ok {
				return
			}
			if filepath.Dir(ev.Name) != lw.adminRoot {
				continue
			}
			switch {
			case ev.Op&fsnotify.Create != 0:
				lw.scheduleCreate(ctx, ev.Name)
			case ev.Op&fsnotify.Remove != 0:
				lw.cancelPendingCreate(ev.Name)
				lw.onRemove(ctx, ev.Name)
			}
		case err, ok := <-lw.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("lifecycle fsnotify error", "repo", lw.repoPath, "err", err)
		}
	}
}

// scheduleCreate debounces the CREATE — git writes the admin dir,
// then a sequence of files (HEAD, commondir, gitdir) — so we don't
// fire setup while the worktree is still being assembled. The
// pending timer is cancellable if a REMOVE for the same path arrives
// inside the window (handles `git worktree add` immediately followed
// by `--force` cleanup, or test fixtures racing).
func (lw *LifecycleWatcher) scheduleCreate(ctx context.Context, adminDir string) {
	lw.pendingMu.Lock()
	if t, ok := lw.pendingCreate[adminDir]; ok {
		t.Stop()
	}
	lw.pendingCreate[adminDir] = time.AfterFunc(lw.debounce, func() {
		lw.pendingMu.Lock()
		delete(lw.pendingCreate, adminDir)
		lw.pendingMu.Unlock()
		lw.onCreate(ctx, adminDir)
	})
	lw.pendingMu.Unlock()
}

func (lw *LifecycleWatcher) cancelPendingCreate(adminDir string) {
	lw.pendingMu.Lock()
	if t, ok := lw.pendingCreate[adminDir]; ok {
		t.Stop()
		delete(lw.pendingCreate, adminDir)
	}
	lw.pendingMu.Unlock()
}

func (lw *LifecycleWatcher) onCreate(ctx context.Context, adminDir string) {
	wtPath, ok := readGitdirFile(adminDir)
	if !ok {
		slog.Warn("lifecycle: gitdir not resolvable", "admin", adminDir)
		return
	}
	if _, err := os.Stat(wtPath); err != nil {
		slog.Warn("lifecycle: working tree missing", "wt", wtPath, "err", err)
		return
	}
	lw.mu.Lock()
	lw.seen[adminDir] = wtPath
	lw.mu.Unlock()

	// CLI coexistence guards. `treeman wt create` shells out to
	// `git worktree add` which fires the same fsnotify CREATE event
	// we're handling here — without these checks the CLI flow and
	// the lifecycle flow both register the worktree and both run
	// setup hooks, fighting on file locks (composer install
	// twice, npm install twice).
	//
	// Two-layer dedup:
	//   1. Active DB row at wtPath → CLI's EnsureWorktree got here
	//      first. CLI is taking responsibility; skip.
	//   2. FinalizeWorktree already in flight for wtPath → another
	//      caller is already running the hooks; skip.
	//
	// True external `git worktree add` (no CLI) trips neither guard:
	// the row doesn't exist yet, no finalize is in flight, so the
	// watcher proceeds to register + finalize.
	if existing, err := lw.st.Store.LookupActiveWorktreeByPath(ctx, wtPath); err == nil && existing.ID != 0 {
		slog.Debug("lifecycle: skip setup (CLI already registered)",
			"wt", wtPath, "id", existing.ID)
		return
	}
	if lw.st.IsFinalizeInFlight(wtPath) {
		slog.Debug("lifecycle: skip setup (finalize already in flight)", "wt", wtPath)
		return
	}

	branch := detectBranch(wtPath)
	sl := slug.For(wtPath, branch)
	if _, err := lw.st.Store.EnsureWorktreeWithAdmin(ctx, lw.repoID, wtPath, sl.Value, branch, adminDir); err != nil {
		slog.Warn("lifecycle: ensure worktree", "wt", wtPath, "err", err)
		return
	}
	slog.Info("lifecycle: setup dispatched", "repo", lw.repoPath, "wt", wtPath)
	// Best-effort env rehydration — when an external `git worktree
	// add` runs without the CLI, there's no captured env yet, so
	// LoadInheritedEnvByPath returns nil and FinalizeWorktree gets
	// an empty env. The next user-driven `wt finalize` will populate
	// the cache.
	env, _ := lw.st.Store.LoadInheritedEnvByPath(ctx, wtPath)
	if env == nil {
		env = map[string]string{}
	}
	if err := FinalizeWorktree(ctx, lw.st, lw.repoPath, wtPath, env); err != nil {
		slog.Warn("lifecycle: setup failed", "wt", wtPath, "err", err)
	}
}

func (lw *LifecycleWatcher) onRemove(ctx context.Context, adminDir string) {
	lw.mu.Lock()
	wtPath := lw.seen[adminDir]
	delete(lw.seen, adminDir)
	lw.mu.Unlock()
	if wtPath == "" {
		row, err := lw.st.Store.LookupWorktreeByAdminDir(ctx, adminDir)
		if err != nil {
			slog.Warn("lifecycle: lookup by admin_dir", "admin", adminDir, "err", err)
			return
		}
		if row.ID == 0 {
			slog.Warn("lifecycle: no worktree row for admin_dir", "admin", adminDir)
			return
		}
		if row.Deleted {
			return
		}
		wtPath = row.Path
	}
	// `treeman wt delete` removes the admin dir via `git worktree
	// remove` near the end of its teardown — that REMOVE fsnotify
	// event lands here. The primary teardown already runs teardown
	// + DB teardown + marks the row deleted; spawning an orphan
	// teardown behind the same per-repo mutex just blocks waiting for
	// the primary to release, then no-ops on the deleted-row check.
	// Skip outright.
	if lw.st.IsTeardownInFlight(wtPath) {
		return
	}
	slog.Info("lifecycle: teardown dispatched", "repo", lw.repoPath, "wt", wtPath)
	if err := teardownOrphan(ctx, lw.st, lw.repoPath, wtPath); err != nil {
		slog.Warn("lifecycle: teardown failed", "wt", wtPath, "err", err)
	}
}

// reconcile diffs the filesystem state against the DB rows for this
// repo at boot:
//
//   - admin_dir present on disk but no DB row → setup dispatch.
//   - DB row with admin_dir but the dir is gone → teardown dispatch.
//
// Closing the loop here covers the "daemon was down when the user ran
// `git worktree add`/`remove`" case. Idempotent — setup logic
// short-circuits when the worktree already has resources, teardown
// is gated on `deleted_at IS NULL`.
func (lw *LifecycleWatcher) reconcile(ctx context.Context) error {
	entries, err := os.ReadDir(lw.adminRoot)
	if err != nil {
		return fmt.Errorf("read %s: %w", lw.adminRoot, err)
	}
	onDisk := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		admin := filepath.Join(lw.adminRoot, e.Name())
		wtPath, ok := readGitdirFile(admin)
		if !ok {
			continue
		}
		onDisk[admin] = wtPath
	}

	dbRows, err := lw.st.Store.ListWorktreesForRepo(ctx, lw.repoID)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	dbByAdmin := map[string]store.WorktreeRow{}
	for _, w := range dbRows {
		if w.AdminDir != "" {
			dbByAdmin[w.AdminDir] = w
		}
	}

	for admin, wtPath := range onDisk {
		lw.mu.Lock()
		lw.seen[admin] = wtPath
		lw.mu.Unlock()
		row, ok := dbByAdmin[admin]
		if !ok || row.Deleted {
			lw.onCreate(ctx, admin)
			continue
		}
		// Row exists and active — make sure admin_dir is stamped
		// even if the row was first inserted via the CLI path that
		// doesn't yet populate it.
		if row.AdminDir == "" {
			_ = lw.st.Store.SetWorktreeAdminDir(ctx, row.ID, admin)
		}
	}

	for admin, row := range dbByAdmin {
		if row.Deleted {
			continue
		}
		if _, ok := onDisk[admin]; ok {
			continue
		}
		lw.onRemove(ctx, admin)
	}
	return nil
}

// teardownOrphan is the "working tree already gone" variant of
// TeardownWorktree. Runs teardown hooks (NOT teardown — the tree
// is gone) cwd'd into the repo root, drops the per-worktree
// databases, and marks the row deleted. Does NOT call `git worktree
// remove` — git already did.
func teardownOrphan(ctx context.Context, st *State, repoPath, wtPath string) error {
	mu := st.LockRepoTeardown(repoPath)
	mu.Lock()
	defer mu.Unlock()

	cfg, err := resolve.LoadResolved(repoPath)
	if err != nil {
		return err
	}
	// Branch/slug from the deleted tree are no longer derivable
	// from the filesystem. Pull the slug from the DB row that the
	// lifecycle CREATE path stamped earlier.
	repoID, err := st.Store.EnsureRepo(ctx, repoPath, filepath.Base(repoPath))
	if err != nil {
		return err
	}
	row, err := lookupWorktreeByPath(ctx, st.Store, wtPath)
	if err != nil {
		return err
	}
	if row.ID == 0 {
		// Unregistered worktree — nothing to tear down.
		return nil
	}
	if row.Deleted {
		return nil
	}

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_lifecycle_teardown_start",
		"lifecycle watcher: teardown hooks + db teardown beginning",
		repoID, row.ID, "", 0, nil)

	// Orphan teardown — the working tree is gone, so we run
	// before-engines + after-engines hooks against a log dir under
	// XDG_STATE_HOME instead of the (deleted) worktree.
	logDir := orphanLogDir(repoPath, row.Slug)
	runOrphan := func(trigger string, actions []config.Action) {
		if len(actions) == 0 {
			return
		}
		started := hooks.EmitHookStart(ctx, st.Store, repoID, row.ID, trigger, len(actions))
		out, _ := hooks.RunHooksOrphan(ctx, trigger, actions,
			repoPath, wtPath, row.Slug, logDir, map[string]string{}, true)
		hooks.PersistOutcome(ctx, st.Store, repoID, row.ID, trigger, started, nowMillis(), out)
	}
	runOrphan("on-delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines)

	if len(cfg.Databases) > 0 {
		// Inline drop so on-delete-after-engines actions observe a
		// fully-dropped state. The lifecycle goroutine already runs
		// detached from the originating fsnotify event, so the cost
		// stays off the user's CLI hot path.
		if err := prepare.TeardownDatabases(ctx, &cfg, row.Slug, repoID, row.ID, st.Store); err != nil {
			slog.Warn("lifecycle teardown DB drop", "wt", wtPath, "err", err)
		}
	}
	runOrphan("on-delete-after-engines", cfg.Hooks.OnDeleteAfterEngines)

	if err := st.Store.MarkWorktreeDeleted(ctx, row.ID); err != nil {
		return err
	}
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, "wt_lifecycle_teardown_done",
		"lifecycle watcher: teardown + db teardown complete",
		repoID, row.ID, "", 0, nil)
	return nil
}

// orphanLogDir routes lifecycle teardown logs to a daemon-managed
// state path since the worktree dir (which would normally host
// `.treeman-hooks/`) no longer exists.
func orphanLogDir(repoPath, slug string) string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		if home, err := os.UserHomeDir(); err == nil {
			state = filepath.Join(home, ".local", "state")
		} else {
			state = filepath.Join(os.TempDir(), "treeman-state")
		}
	}
	return filepath.Join(state, "treeman", "lifecycle-logs",
		filepath.Base(repoPath), slug+"-"+time.Now().UTC().Format("20060102T150405Z"))
}

// lookupWorktreeByPath is the orphan-teardown helper: we only have a
// path (the deleted worktree's filesystem location). The most recent
// matching row is the right one — an old removed row for the same
// path would have `deleted_at` set and we skip it.
func lookupWorktreeByPath(ctx context.Context, s *store.Store, path string) (store.WorktreeRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), deleted_at IS NOT NULL
		FROM worktrees WHERE path = ? ORDER BY id DESC LIMIT 1`, path)
	if err != nil {
		return store.WorktreeRow{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return store.WorktreeRow{}, nil
	}
	var w store.WorktreeRow
	if err := rows.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &w.Deleted); err != nil {
		return store.WorktreeRow{}, err
	}
	return w, rows.Err()
}

// resolveGitCommonDir returns the absolute path of the repo's git
// common directory (where `worktrees/` lives). For the main worktree
// of a repo with a normal `.git` directory, this is `<repo>/.git`.
// For a repo cloned with `--separate-git-dir` (or a linked worktree
// passed as `repoPath`), follow `.git` as a gitlink file.
func resolveGitCommonDir(repoPath string) (string, error) {
	dot := filepath.Join(repoPath, ".git")
	info, err := os.Stat(dot)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return dot, nil
	}
	// gitlink case: `.git` is a file containing `gitdir: <path>`.
	b, err := os.ReadFile(dot)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("unexpected .git file content: %q", line)
	}
	gitdir := strings.TrimSpace(line[len(prefix):])
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(repoPath, gitdir)
	}
	// For a linked worktree `gitdir` is `<common>/worktrees/<name>` —
	// strip those two segments to get the common dir.
	parent := filepath.Dir(gitdir)
	if filepath.Base(parent) == "worktrees" {
		return filepath.Dir(parent), nil
	}
	return gitdir, nil
}

// readGitdirFile reads `<adminDir>/gitdir`, which git writes when a
// linked worktree is created. The file's content is the absolute path
// of the working tree's `.git` gitlink file — strip the trailing
// `/.git` to get the working tree root.
func readGitdirFile(adminDir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		return "", false
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return "", false
	}
	if strings.HasSuffix(line, "/.git") {
		return strings.TrimSuffix(line, "/.git"), true
	}
	return filepath.Dir(line), true
}
