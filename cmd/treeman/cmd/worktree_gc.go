package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
	"golang.org/x/sync/errgroup"

	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wt"
)

// gcCacheDirNames are the heavyweight, regenerable directories
// `worktree gc --caches-only` removes inside a worktree while keeping
// the checkout, branch, and registry row. A later finalize rebuilds
// them. storage/ is treeman's per-worktree dump dir on Laravel-style
// layouts (multi-GB) and is the single biggest win.
var gcCacheDirNames = []string{
	"node_modules", "vendor", "storage", "var", "tmp", "coverage",
	"dist", "build", "target", ".cache", ".next", ".turbo", "venv", ".venv",
}

// gcCandidate is one planned reclaim target.
type gcCandidate struct {
	row      store.WorktreeRow
	repoPath string
	// action is "delete" (full teardown) or "caches" (drop the
	// regenerable dirs, keep the checkout).
	action string
	reason string
	// bytes is the reclaimable size (whole tree for delete, cache dirs
	// for caches).
	bytes int64
	// lastEventMs is the newest event ts (0 = none).
	lastEventMs int64
}

// WtGcCmd — `treeman worktree gc`. Reclaims disk from merged/stale
// worktrees: full teardown for branches already contained in the
// default branch, cache-dir removal for stale-but-live ones.
func WtGcCmd() *cli.Command {
	return &cli.Command{
		Name:  "gc",
		Usage: "reclaim disk from merged/stale worktrees (delete merged, drop caches of stale)",
		Description: `Plans and executes a reclaim across every registered repo
(or one repo with --repo):

  --merged     worktrees whose branch is contained in the default
               branch (or --merged=<ref>) are DELETED (full teardown:
               hooks, databases, git worktree remove, registry row).
               Dirty or unpushed worktrees are skipped with a warning.
  --stale <d>  worktrees with no activity (events, last visit, HEAD
               commit) within <d> (e.g. 30d) get their regenerable
               cache dirs removed (node_modules, vendor, storage, …) —
               the checkout, branch, and registry row stay; a later
               finalize rebuilds them.

With no selector, --stale 30d applies (the safe default). --dry-run
prints the plan and reclaimable bytes without touching anything.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "scope to one repo (path)"},
			&cli.StringFlag{
				Name:  "merged",
				Usage: "delete worktrees whose branch is merged into the default branch (or =<ref>)",
			},
			&cli.StringFlag{Name: "stale", Usage: "duration without activity after which caches are reclaimed (e.g. 30d; '' disables)"},
			&cli.BoolFlag{Name: "dry-run", Usage: "print the plan + reclaimable bytes; change nothing"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		},
		Action: runWorktreeGc,
	}
}

func runWorktreeGc(ctx context.Context, c *cli.Command) error {
	stale, deleteMerged, mergedRef, err := gcSelectors(c.String("stale"), c.String("merged"), c.IsSet("merged"))
	if err != nil {
		return err
	}

	st, err := openSharedStore(ctx)
	if err != nil {
		return err
	}

	rows, err := st.ListActiveWorktreeRows(ctx)
	if err != nil {
		return err
	}
	if repo := c.String("repo"); repo != "" {
		rows = filterWorktreeRows(rows, MustAbs(repo))
	}

	cands, skipped := gcPlan(ctx, st, rows, gcOptions{
		stale:        stale,
		deleteMerged: deleteMerged,
		mergedRef:    mergedRef,
	})
	if len(cands) == 0 {
		ui.Info("nothing to reclaim (%d worktree(s) considered, %d skipped)", len(rows), skipped)
		return nil
	}

	var total int64
	for _, cand := range cands {
		ui.Plain("%s  %-6s  %s  %s  %s",
			ui.Cyan(cand.action),
			gcStaleLabel(cand.lastEventMs),
			ui.Dim(humanBytes(cand.bytes)),
			cand.row.Branch,
			ui.Dim(cand.row.Path))
		total += cand.bytes
	}
	ui.Plain("")
	ui.Info("reclaimable: %s across %d worktree(s) (%d skipped)", humanBytes(total), len(cands), skipped)

	if c.Bool("dry-run") {
		return nil
	}
	if !c.Bool("yes") && !ui.ConfirmYes(fmt.Sprintf("Reclaim %s?", humanBytes(total))) {
		PrintInfo("aborted: nothing touched")
		return nil
	}
	return gcExecute(ctx, cands)
}

// gcSelectors normalizes the selector flags: --stale defaults to 30d
// when no selector was given at all, --merged may carry an optional
// ref value (--merged or --merged=origin/master).
func gcSelectors(staleStr, mergedStr string, mergedSet bool) (stale time.Duration, deleteMerged bool, mergedRef string, err error) {
	if staleStr == "" && mergedStr == "" && !mergedSet {
		staleStr = "30d"
	}
	if staleStr != "" {
		stale, err = parseDaysDuration(staleStr)
		if err != nil {
			return 0, false, "", fmt.Errorf("--stale: %w", err)
		}
	}
	if mergedSet {
		deleteMerged = true
		mergedRef = mergedStr
	}
	return stale, deleteMerged, mergedRef, nil
}

// parseDaysDuration accepts Go durations ("72h") plus an "Nd" day
// suffix ("30d") — the natural unit for worktree staleness.
func parseDaysDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func filterWorktreeRows(rows []store.WorktreeRow, repo string) []store.WorktreeRow {
	out := rows[:0:0]
	for _, r := range rows {
		if strings.EqualFold(r.RepoPath, repo) {
			out = append(out, r)
		}
	}
	return out
}

type gcOptions struct {
	stale        time.Duration
	deleteMerged bool
	mergedRef    string
}

// gcPlan builds the reclaim plan: merged branches (when selected) are
// delete candidates, stale worktrees are caches-only candidates. The
// main checkout, the cwd worktree, missing directories, dirty/unpushed
// delete candidates, and worktrees mid-teardown are skipped and
// counted.
func gcPlan(ctx context.Context, st *store.Store, rows []store.WorktreeRow, opts gcOptions) ([]gcCandidate, int) {
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	lastEvent, _ := st.LastEventTsPerWorktree(ctx, ids)

	// mergedSet: branch → merged, per repo, one for-each-ref per repo.
	mergedSets := map[string]map[string]bool{}
	mergedRefs := map[string]string{}
	now := time.Now()
	cwd, _ := os.Getwd()

	mergedSetFor := func(repoPath string) (map[string]bool, string) {
		if set, ok := mergedSets[repoPath]; ok {
			return set, mergedRefs[repoPath]
		}
		ref := opts.mergedRef
		if ref == "" {
			ref = "origin/" + wt.DetectDefaultBranch(ctx, repoPath)
		}
		set := mergedIntoRef(ctx, repoPath, ref)
		mergedSets[repoPath] = set
		mergedRefs[repoPath] = ref
		return set, ref
	}

	var cands []gcCandidate
	skipped := 0
	for _, row := range rows {
		if row.IsMain {
			skipped++ // never reclaim the main checkout
			continue
		}
		if _, err := os.Stat(row.Path); err != nil {
			skipped++ // already gone — `worktree prune` territory
			continue
		}
		if strings.EqualFold(filepath.Clean(row.Path), filepath.Clean(cwd)) {
			skipped++ // the shell is standing in it
			continue
		}

		cand := gcCandidate{row: row, repoPath: row.RepoPath, lastEventMs: lastEvent[row.ID]}

		if opts.deleteMerged {
			set, ref := mergedSetFor(row.RepoPath)
			if cand, ok := gcMergedCandidate(ctx, cand, set, ref); ok {
				cands = append(cands, cand)
				continue
			} else if cand.action == "skip" {
				skipped++ // merged but dirty/unpushed — warned inside
				continue
			}
		}

		if opts.stale > 0 && gcStale(cand, now, opts.stale) {
			cand.action = "caches"
			cand.reason = fmt.Sprintf("no activity for %s", staleAge(cand, now).Round(24*time.Hour))
			cand.bytes = cacheDirsBytes(row.Path)
			if cand.bytes > 0 {
				cands = append(cands, cand)
			} else {
				skipped++ // nothing regenerable to reclaim
			}
			continue
		}
		skipped++
	}
	return cands, skipped
}

// gcMergedCandidate decides the delete arm for one row: a merged,
// clean, fully-pushed worktree becomes a delete candidate (ok=true);
// a merged-but-dirty/unpushed one is skipped with a warning (the
// returned cand carries action="skip", ok=false); an unmerged row
// falls through to the other arms (action="", ok=false).
func gcMergedCandidate(ctx context.Context, cand gcCandidate, merged map[string]bool, ref string) (gcCandidate, bool) {
	if !merged[cand.row.Branch] {
		return cand, false
	}
	// Batch safety: never delete dirty or unpushed work.
	if dirty, _ := gitenv.HasWorkingTreeChanges(ctx, cand.row.Path); dirty {
		ui.Warn("skip %s: merged but dirty (uncommitted changes)", cand.row.Path)
		cand.action = "skip"
		return cand, false
	}
	if unpushed, _ := gitenv.HasUnpushedCommits(ctx, cand.row.Path); unpushed {
		ui.Warn("skip %s: merged but has unpushed commits", cand.row.Path)
		cand.action = "skip"
		return cand, false
	}
	cand.action = "delete"
	cand.reason = "merged into " + ref
	cand.bytes = treeBytes(ctx, cand.row.Path)
	return cand, true
}

// gcStale reports whether the worktree has seen no activity within the
// window: newest of last event, last visit, and HEAD commit time.
func gcStale(c gcCandidate, now time.Time, window time.Duration) bool {
	return staleAge(c, now) > window
}

func staleAge(c gcCandidate, now time.Time) time.Duration {
	last := time.Time{}
	if c.lastEventMs > 0 {
		last = time.UnixMilli(c.lastEventMs)
	}
	if ts := headCommitTs(c.row.Path); ts > 0 {
		if t := time.Unix(ts, 0); t.After(last) {
			last = t
		}
	}
	if c.row.LastVisitedMs > 0 {
		if t := time.UnixMilli(c.row.LastVisitedMs); t.After(last) {
			last = t
		}
	}
	if last.IsZero() {
		return time.Duration(1 << 62) // never seen: maximally stale
	}
	return now.Sub(last)
}

// mergedIntoRef returns the set of local branches contained in ref, in
// one fork.
func mergedIntoRef(ctx context.Context, repoRoot, ref string) map[string]bool {
	out, err := gitcmd.Output(ctx, repoRoot, "for-each-ref",
		"--merged="+ref, "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return map[string]bool{}
	}
	set := map[string]bool{}
	for name := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			set[name] = true
		}
	}
	return set
}

// gcExecute runs the plan: deletes go through the normal wt.Delete
// teardown (hooks + databases + git worktree remove + registry row);
// caches candidates just remove the regenerable dirs.
func gcExecute(ctx context.Context, cands []gcCandidate) error {
	var errs []error
	for _, cand := range cands {
		switch cand.action {
		case "delete":
			if _, err := wt.Delete(ctx, wt.DeleteRequest{
				RepoRoot: cand.repoPath,
				Target:   cand.row.Path,
				Env:      CaptureInheritedEnv(),
			}, cliSink{}); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", cand.row.Path, err))
			} else {
				PrintOK("deleted %s (%s)", cand.row.Path, cand.reason)
			}
		case "caches":
			reclaimed, err := dropCacheDirs(cand.row.Path)
			if err != nil {
				errs = append(errs, fmt.Errorf("caches %s: %w", cand.row.Path, err))
				continue
			}
			PrintOK("dropped caches in %s (%s reclaimed, %s)", cand.row.Path, humanBytes(reclaimed), cand.reason)
		}
	}
	return errors.Join(errs...)
}

// dropCacheDirs removes the regenerable heavy dirs from a worktree and
// returns how many bytes that freed.
func dropCacheDirs(worktree string) (int64, error) {
	var total int64
	for _, name := range gcCacheDirNames {
		dir := filepath.Join(worktree, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		size, serr := dirSize(dir)
		if serr == nil {
			total += size
		}
		if err := os.RemoveAll(dir); err != nil {
			return total, err
		}
	}
	return total, nil
}

// cacheDirsBytes sums the regenerable dirs' sizes (0 when none exist).
func cacheDirsBytes(worktree string) int64 {
	var total int64
	for _, name := range gcCacheDirNames {
		dir := filepath.Join(worktree, name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if size, err := dirSize(dir); err == nil {
			total += size
		}
	}
	return total
}

// treeBytes is the whole-worktree on-disk size.
func treeBytes(ctx context.Context, path string) int64 {
	out, err := exec.CommandContext(ctx, "du", "-sk", path).Output()
	if err != nil {
		return 0
	}
	kb, err := strconv.ParseInt(strings.Fields(string(out))[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

// dirSize walks dir and sums regular file sizes. Cheaper than forking
// du for the handful of cache dirs.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries just don't count
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

func gcStaleLabel(lastEventMs int64) string {
	if lastEventMs == 0 {
		return "—"
	}
	age := time.Since(time.UnixMilli(lastEventMs)).Round(24 * time.Hour)
	return age.String()
}

// humanBytes renders a byte count with one decimal (3.2 GB).
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// worktreeSizes computes per-path on-disk sizes concurrently. Missing
// paths report 0.
func worktreeSizes(ctx context.Context, paths []string) []int64 {
	sizes := make([]int64, len(paths))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, p := range paths {
		g.Go(func() error {
			sizes[i] = treeBytes(gctx, p)
			return nil
		})
	}
	_ = g.Wait()
	return sizes
}
