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
	"github.com/stubbedev/treeman/internal/fspath"
	"github.com/stubbedev/treeman/internal/hooks"
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

// startLifecycleWatcher subscribes to a repo's git common dir and its
// `worktrees/` directory and runs an initial reconcile pass against the
// DB. A second call for the same repoPath cancels the prior watcher.
func startLifecycleWatcher(ctx context.Context, st *State, repoID int64, repoPath string) error {
	if repoPath == "" {
		return errors.New("lifecycle: empty repo_path")
	}
	commonDir, err := resolveGitCommonDir(repoPath)
	if err != nil {
		return fmt.Errorf("lifecycle: resolve common-dir: %w", err)
	}
	adminRoot := filepath.Join(commonDir, "worktrees")
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("lifecycle: fsnotify new: %w", err)
	}
	if err := fsw.Add(commonDir); err != nil {
		_ = fsw.Close()
		return fmt.Errorf("lifecycle: add %s: %w", commonDir, err)
	}
	if err := fsw.Add(adminRoot); err != nil && !errors.Is(err, fs.ErrNotExist) {
		_ = fsw.Close()
		return fmt.Errorf("lifecycle: add %s: %w", adminRoot, err)
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
	safeGo(lblLifecycle, repoPath, func() { lw.loop(wctx) })
	slog.Info("lifecycle watcher started", "repo", repoPath, "admin_root", adminRoot)
	return nil
}

func EnsureLifecycleWatcher(ctx context.Context, st *State, repoID int64, repoPath string) error {
	if st.HasLifecycleWatcher(repoPath) {
		return nil
	}
	return startLifecycleWatcher(ctx, st, repoID, repoPath)
}

func (lw *LifecycleWatcher) rearmAdminRoot(ctx context.Context) {
	if err := lw.fsw.Add(lw.adminRoot); err != nil {
		slog.Warn("lifecycle: re-watch admin root", "repo", lw.repoPath, "admin_root", lw.adminRoot, "err", err)
		return
	}
	entries, err := os.ReadDir(lw.adminRoot)
	if err != nil {
		slog.Warn("lifecycle: read admin root", "repo", lw.repoPath, "admin_root", lw.adminRoot, "err", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			lw.scheduleCreate(ctx, filepath.Join(lw.adminRoot, e.Name()))
		}
	}
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
			if ev.Name == lw.adminRoot {
				if ev.Op&fsnotify.Create != 0 {
					lw.rearmAdminRoot(ctx)
				}
				continue
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
	wtPath, ok, err := readGitdirFile(adminDir)
	if err != nil {
		slog.Error("lifecycle: skip setup", "admin", adminDir, "err", err)
		return
	}
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

	// CLI coexistence guards. `treeman worktree create` shells out to
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
	existing, err := lw.st.Store.LookupActiveWorktreeByPath(ctx, wtPath)
	if err != nil {
		slog.Error("lifecycle: skip setup (cannot tell whether the CLI already registered it)", "wt", wtPath, "err", err)
		return
	}
	if existing.ID != 0 {
		slog.Debug("lifecycle: skip setup (CLI already registered)",
			"wt", wtPath, "id", existing.ID)
		return
	}
	if lw.st.IsFinalizeInFlight(wtPath) {
		slog.Debug("lifecycle: skip setup (finalize already in flight)", "wt", wtPath)
		return
	}
	// A daemon worktree_create for this repo is still between `git
	// worktree add` and registering its row (a large checkout outlasts
	// the debounce). Retry after another debounce instead of racing it:
	// once the row lands, guard 1 skips; a genuinely external add is
	// picked up as soon as the create finishes (#122).
	if lw.st.IsCreateInFlight(lw.repoPath) {
		slog.Debug("lifecycle: defer setup (create in flight)", "wt", wtPath)
		lw.scheduleCreate(ctx, adminDir)
		return
	}

	branch := detectBranch(wtPath)
	sl := slug.For(wtPath, branch)
	if _, err := lw.st.Store.EnsureWorktreeWithAdmin(ctx, lw.repoID, wtPath, sl.Value, branch, adminDir); err != nil {
		slog.Warn("lifecycle: ensure worktree", "wt", wtPath, "err", err)
		return
	}
	slog.Info("lifecycle: setup dispatched", "repo", lw.repoPath, "wt", wtPath)
	env, ok := inheritedEnvOrSkip(ctx, lw.st, lw.repoID, wtPath, "setup of the externally added worktree")
	if !ok {
		return
	}
	if env == nil {
		env = map[string]string{}
	}
	appeared := withTrigger(ctx, trigger{Source: triggerWorktreeAppeared})
	if err := FinalizeWorktree(appeared, lw.st, lw.repoPath, wtPath, env, false); err != nil {
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
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", lw.adminRoot, err)
	}
	onDisk, errs := lw.adminDirsOnDisk(entries)

	dbRows, err := lw.st.Store.ListWorktreesForRepo(ctx, lw.repoID)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	dbByAdmin := map[string]store.WorktreeRow{}
	unstampedByPath := map[string]store.WorktreeRow{}
	for _, w := range dbRows {
		switch {
		case w.AdminDir != "":
			dbByAdmin[w.AdminDir] = w
		case !w.Deleted:
			unstampedByPath[worktreePathKey(w.Path)] = w
		}
	}

	// Fan out onCreate / onRemove across reconcileWorkers goroutines.
	// Each onCreate calls FinalizeWorktree (db setup + hook chains),
	// each onRemove calls teardownOrphan (db teardown + hook chains) —
	// on boot of a daemon with N stale worktrees, the old serial loop
	// stalled the constructor for N×finalize-time before the event
	// loop could serve its first RPC.
	const reconcileWorkers = 4
	var wg sync.WaitGroup
	sem := make(chan struct{}, reconcileWorkers)

	for admin, wtPath := range onDisk {
		lw.mu.Lock()
		lw.seen[admin] = wtPath
		lw.mu.Unlock()
		row, ok := dbByAdmin[admin]
		if unstamped, found := unstampedByPath[worktreePathKey(wtPath)]; !ok && found {
			if err := lw.st.Store.SetWorktreeAdminDir(ctx, unstamped.ID, admin); err != nil {
				errs = append(errs, fmt.Errorf("record admin dir %s for worktree %s: %w", admin, wtPath, err))
			}
			continue
		}
		if !ok || row.Deleted {
			wg.Add(1)
			sem <- struct{}{}
			safeGo(lblLifecycle, admin, func() {
				defer wg.Done()
				defer func() { <-sem }()
				lw.onCreate(ctx, admin)
			})
		}
	}

	for admin, row := range dbByAdmin {
		if row.Deleted {
			continue
		}
		if _, ok := onDisk[admin]; ok {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		safeGo(lblLifecycle, admin, func() {
			defer wg.Done()
			defer func() { <-sem }()
			lw.onRemove(ctx, admin)
		})
	}
	errs = append(errs, lw.reapUnstamped(ctx, unstampedByPath, onDisk, &wg, sem)...)
	wg.Wait()
	return errors.Join(errs...)
}

func (lw *LifecycleWatcher) adminDirsOnDisk(entries []fs.DirEntry) (map[string]string, []error) {
	onDisk := map[string]string{}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		admin := filepath.Join(lw.adminRoot, e.Name())
		wtPath, ok, err := readGitdirFile(admin)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			onDisk[admin] = wtPath
		}
	}
	return onDisk, errs
}

func (lw *LifecycleWatcher) reapUnstamped(
	ctx context.Context,
	unstampedByPath map[string]store.WorktreeRow,
	onDisk map[string]string,
	wg *sync.WaitGroup,
	sem chan struct{},
) []error {
	onDiskPaths := make(map[string]bool, len(onDisk))
	for _, p := range onDisk {
		onDiskPaths[worktreePathKey(p)] = true
	}
	var errs []error
	for key, row := range unstampedByPath {
		path := row.Path
		if row.IsMain || onDiskPaths[key] {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("check whether worktree %s still exists: %w", path, err))
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		safeGo(lblLifecycle, path, func() {
			defer wg.Done()
			defer func() { <-sem }()
			if lw.st.IsTeardownInFlight(path) {
				return
			}
			slog.Info("lifecycle: teardown dispatched", "repo", lw.repoPath, "wt", path)
			if err := teardownOrphan(ctx, lw.st, lw.repoPath, path); err != nil {
				slog.Warn("lifecycle: teardown failed", "wt", path, "err", err)
			}
		})
	}
	return errs
}

// teardownOrphan is the "working tree already gone" variant of
// TeardownWorktree. Runs teardown hooks (NOT teardown — the tree
// is gone) cwd'd into the repo root, drops the per-worktree
// databases, and marks the row deleted. Does NOT call `git worktree
// remove` — git already did.
func teardownOrphan(ctx context.Context, st *State, repoPath, wtPath string) (err error) {
	if !st.MarkTeardownInFlight(wtPath) {
		return nil
	}
	defer st.UnmarkTeardownInFlight(wtPath)

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

	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeReapStart,
		"lifecycle watcher: teardown hooks + db teardown beginning",
		repoID, row.ID, "", 0, nil)
	defer func() {
		if err != nil {
			_ = st.Store.WriteEvent(context.WithoutCancel(ctx), store.LevelError, store.EvtWorktreeReapError,
				err.Error(), repoID, row.ID, "", 0, map[string]string{"worktree_path": wtPath})
		}
	}()

	// Orphan teardown — the working tree is gone, so we run
	// before-engines + after-engines hooks against a log dir under
	// XDG_STATE_HOME instead of the (deleted) worktree.
	logDir := hooks.OrphanLogDir(repoPath, row.Slug)
	runOrphan := func(trigger string, actions []config.Action) {
		if len(actions) == 0 {
			return
		}
		started := hooks.EmitHookStart(ctx, st.Store, repoID, row.ID, trigger, len(actions))
		out, runErr := hooks.RunHooksOrphan(ctx, trigger, actions,
			repoPath, wtPath, row.Slug, logDir, row.IsMain, map[string]string{}, true)
		if runErr != nil {
			slog.Warn("lifecycle teardown: hooks failed; teardown continues", "trigger", trigger, "wt", wtPath, "err", runErr)
		}
		hooks.PersistOutcome(ctx, st.Store, repoID, row.ID, trigger, started, nowMillis(), out)
	}
	runOrphan("delete-before-engines", cfg.Hooks.OnDeleteBeforeEngines)

	if len(cfg.Databases) > 0 {
		// Inline drop so delete-after-engines actions observe a
		// fully-dropped state. The lifecycle goroutine already runs
		// detached from the originating fsnotify event, so the cost
		// stays off the user's CLI hot path.
		if err := teardownDatabasesWithHeartbeat(ctx, &cfg, row.Slug, repoID, row.ID, st.Store); err != nil {
			slog.Warn("lifecycle teardown DB drop", "wt", wtPath, "err", err)
		}
	}
	runOrphan("delete-after-engines", cfg.Hooks.OnDeleteAfterEngines)

	// Physically release the port reservations,
	// same as the CLI delete path. A bare MarkWorktreeDeleted leaves the
	// worktree_ports rows in place; the unique index on (repo_id, name,
	// port) then blocks the next worktree from re-using the freed port.
	if err := st.Store.ReleaseWorktreePorts(ctx, row.ID); err != nil {
		slog.Warn("release worktree ports (freed ports stay blocked for reallocation)", "wt", wtPath, "err", err)
	}

	if err := st.Store.MarkWorktreeDeleted(ctx, row.ID); err != nil {
		return err
	}
	_ = st.Store.WriteEvent(ctx, store.LevelInfo, store.EvtWorktreeReapEnd,
		"lifecycle watcher: teardown + db teardown complete",
		repoID, row.ID, "", 0, nil)
	return nil
}

// lookupWorktreeByPath is the orphan-teardown helper: we only have a
// path (the deleted worktree's filesystem location).
func lookupWorktreeByPath(ctx context.Context, s *store.Store, path string) (store.WorktreeRow, error) {
	return s.LookupWorktreeByPath(ctx, path)
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
func worktreePathKey(p string) string {
	return strings.ToLower(fspath.Canonical(p))
}

func readGitdirFile(adminDir string) (string, bool, error) {
	p := filepath.Join(adminDir, "gitdir")
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", p, err)
	}
	line := strings.TrimSpace(string(b))
	if line == "" {
		return "", false, nil
	}
	if before, ok := strings.CutSuffix(line, "/.git"); ok {
		return before, true, nil
	}
	return filepath.Dir(line), true, nil
}
