package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/gitcmd"
	"github.com/stubbedev/treeman/internal/prepare"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// MainCmd — `treeman main` subcommands flip the repo's main worktree
// into / out of the watcher-driven prepare/migrate/teardown lifecycle.
// The repo's `.treeman.yaml` is the source of truth; this CLI just
// patches the `main_worktree:` block and asks the daemon to reload.
func MainCmd() *cli.Command {
	return &cli.Command{
		Name:  "main",
		Usage: "manage main-worktree enrollment (per-branch DBs at repo root)",
		Commands: []*cli.Command{
			{
				Name:  "enable",
				Usage: "opt the repo root into the watcher lifecycle",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
				},
				Action: mainEnableAction,
			},
			{
				Name:  "disable",
				Usage: "remove the repo root from the watcher lifecycle",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "purge", Usage: "after disabling, drop every per-branch DB the main_<branch> slug owns (current branch + every local branch). Engine resources only — the worktrees row stays soft-deleted for resurrection."},
				},
				Action: mainDisableAction,
			},
			{
				Name:  "status",
				Usage: "show main-worktree enrollment state",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "json"},
				},
				Action: mainStatusAction,
			},
		},
	}
}

func mainEnableAction(ctx context.Context, c *cli.Command) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	if err := patchMainWorktreeConfig(repoRoot, true); err != nil {
		return err
	}
	if err := requestConfigReload(ctx, repoRoot); err != nil {
		ui.Warn("daemon reload failed: %v", err)
		ui.Hint("config written; restart treemand to apply")
		return nil
	}
	// Kick off setup hooks + prepare for the freshly-enrolled main row.
	// The reload above is synchronous: by the time it returns, Enroll
	// MainWorktree has run and the row exists, so FinalizeWorktree
	// routes through the main-wt path (slug.ForMain + overlay) and
	// fires on-create-* hooks with $TREEMAN_IS_MAIN=1.
	if err := requestWorktreeFinalize(ctx, repoRoot, repoRoot); err != nil {
		ui.Warn("daemon finalize dispatch failed: %v", err)
		ui.Hint("run `treeman wt finalize .` at the repo root to retry")
		return nil
	}
	PrintOK("main worktree enabled (%s) — setup hooks + prepare queued (see `treeman logs tail --follow`)", repoRoot)
	return nil
}

func mainDisableAction(ctx context.Context, c *cli.Command) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	// Snapshot the config BEFORE patching — purge needs the engine
	// connections + database templates that we're about to disable.
	var cfgForPurge *config.Config
	if c.Bool("purge") {
		cfg, err := resolve.LoadResolved(repoRoot)
		if err != nil {
			return fmt.Errorf("load config for purge: %w", err)
		}
		cfgForPurge = &cfg
	}
	if err := patchMainWorktreeConfig(repoRoot, false); err != nil {
		return err
	}
	if err := requestConfigReload(ctx, repoRoot); err != nil {
		ui.Warn("daemon reload failed: %v", err)
		ui.Hint("config written; restart treemand to apply")
		return nil
	}
	if cfgForPurge != nil {
		if err := purgeMainDatabases(ctx, repoRoot, cfgForPurge); err != nil {
			ui.Warn("purge: %v", err)
			ui.Hint("config disabled; re-run `treeman main disable --purge` after fixing the engine reachability issue")
			return nil
		}
	}
	PrintOK("main worktree disabled (%s)", repoRoot)
	return nil
}

// purgeMainDatabases drops every per-branch DB the main_<branch>
// slug owns across all local branches plus the current HEAD. Each
// branch's databases are torn down by rendering slug.ForMain and
// invoking prepare.TeardownDatabases with the main-wt overlay
// applied. Errors per-branch are logged but don't abort the rest —
// a branch whose engine resources are already gone shouldn't block
// purging the rest.
//
// Best-effort enumeration: only local branches surface here. A
// branch that was deleted between visits already has its slug-keyed
// DB orphaned today; that pre-existing leak isn't this command's
// problem to chase.
func purgeMainDatabases(ctx context.Context, repoRoot string, cfg *config.Config) error {
	dbPath, _ := store.DefaultDBPath()
	if dbPath == "" {
		return fmt.Errorf("no default db path")
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()
	repoID, _ := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	// Apply the main-wt overlay so prepare.TeardownDatabases sees the
	// same templates the main-wt finalize path used to create the
	// resources. Without the overlay, an overlay-only `name_template`
	// override leaks DBs because the teardown probes the wrong name.
	config.ApplyMainWorktreeOverlay(cfg)

	branches := localBranches(ctx, repoRoot)
	current := detectBranchOfWorktree(repoRoot)
	if current != "" {
		// Make sure HEAD is covered even if for-each-ref didn't
		// surface it (detached HEAD edge cases).
		seen := false
		for _, b := range branches {
			if b == current {
				seen = true
				break
			}
		}
		if !seen {
			branches = append(branches, current)
		}
	}
	if len(branches) == 0 {
		ui.Info("purge: no local branches to enumerate")
		return nil
	}

	var purged int
	for _, branch := range branches {
		sl := slug.ForMain(repoRoot, branch)
		if err := prepare.TeardownDatabases(ctx, cfg, sl.Value, repoID, 0, st); err != nil {
			ui.Warn("purge %s: %v", sl.Value, err)
			continue
		}
		purged++
	}
	ui.Info("purge: tore down DBs for %d branch(es)", purged)
	return nil
}

// localBranches returns the repo's local branches via `git -C repo
// for-each-ref`. Empty slice on error — purge is best-effort.
func localBranches(ctx context.Context, repoRoot string) []string {
	out, err := gitcmd.String(ctx, repoRoot,
		"for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches
}

func mainStatusAction(ctx context.Context, c *cli.Command) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	cfg, err := resolve.LoadResolved(repoRoot)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	row, branch := lookupMainStatus(ctx, repoRoot)

	if c.Bool("json") {
		return jsonStream(map[string]any{
			"repo":           repoRoot,
			"enabled":        cfg.MainWorktree.Enabled,
			"row_id":         row.ID,
			"slug":           row.Slug,
			"branch":         row.Branch,
			"current_branch": branch,
		})
	}

	fmt.Printf("%s\n", ui.Bold(repoRoot))
	fmt.Printf("  enabled        : %v\n", cfg.MainWorktree.Enabled)
	fmt.Printf("  current branch : %s\n", branch)
	if row.ID == 0 {
		fmt.Printf("  enrolled        : no row\n")
		return nil
	}
	fmt.Printf("  enrolled slug   : %s (row %d)\n", row.Slug, row.ID)
	fmt.Printf("  enrolled branch : %s\n", row.Branch)
	return nil
}

// patchMainWorktreeConfig surgically rewrites the `main_worktree:`
// block in `.treeman.yaml`, preserving surrounding comments. Falls
// back to creating the file with a minimal `main_worktree:` stanza
// when the repo has no .treeman.yaml yet — a virgin repo wanting to
// opt-in shouldn't need to scaffold a full config first.
func patchMainWorktreeConfig(repoRoot string, enabled bool) error {
	target := filepath.Join(repoRoot, ".treeman.yaml")
	raw, err := os.ReadFile(target)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read %s: %w", target, err)
		}
		raw = nil
	}

	var doc yaml.Node
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", target, err)
		}
	}
	if doc.Kind == 0 {
		// Empty file or missing — seed an empty mapping document the
		// yamlpatch helpers can splice into.
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}

	enabledSegs, err := yamlpatch.ParsePath("main_worktree.enabled")
	if err != nil {
		return err
	}
	enabledNode, _ := yamlpatch.ValueToNode(enabled)
	if _, err := yamlpatch.Set(&doc, enabledSegs, enabledNode); err != nil {
		return err
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return err
	}
	return yamlpatch.AtomicWriteWithBackup(target, body, 5)
}

// requestWorktreeFinalize dispatches a finalize RPC for wtPath under
// repoRoot. Used by `main enable` to kick off on-create-* hooks +
// prepare against the freshly-enrolled main row without making the
// user manually run `treeman wt finalize .`. The CLI captures the
// user's env here because the daemon goroutine wouldn't otherwise
// see PATH / nvm shims / etc.
func requestWorktreeFinalize(ctx context.Context, repoRoot, wtPath string) error {
	resp, err := rpc.Call(ctx, rpc.Request{
		Method: rpc.MethodWorktreeFinalize,
		WorktreeFinalize: &rpc.WorktreeFinalizeArgs{
			RepoPath:     repoRoot,
			WorktreePath: wtPath,
			InheritedEnv: CaptureInheritedEnv(),
		},
	})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil
}

// requestConfigReload tells the daemon to re-evaluate this repo's
// resolved config so the new `main_worktree:` block takes effect
// without a daemon restart. Tolerates a non-running daemon — the
// caller's CLI side has already written the YAML.
func requestConfigReload(ctx context.Context, repoRoot string) error {
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:       rpc.MethodConfigReload,
		ConfigReload: &rpc.ConfigReloadArgs{RepoPath: repoRoot},
	})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	return nil
}

// lookupMainStatus reads the main-wt row (if any) for repoRoot and
// the current branch. Errors collapse to zero-values so status never
// fails on a missing daemon DB — the caller renders "no row".
func lookupMainStatus(ctx context.Context, repoRoot string) (store.WorktreeRow, string) {
	branch := detectBranchOfWorktree(repoRoot)
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return store.WorktreeRow{}, branch
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return store.WorktreeRow{}, branch
	}
	defer func() { _ = st.Close() }()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil || repoID == 0 {
		return store.WorktreeRow{}, branch
	}
	row, _ := st.LookupMainWorktree(ctx, repoID)
	return row, branch
}
