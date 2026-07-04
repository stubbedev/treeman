package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
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
					&cli.BoolFlag{
						Name:  "purge",
						Usage: "after disabling, drop every per-branch DB the main_<branch> slug owns (current branch + every local branch). Engine resources only — the worktrees row stays soft-deleted for resurrection.",
					},
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
	// patchMainWorktreeConfig writes via the daemon, which reloads the
	// repo as part of the same task — so by the time it returns,
	// EnrollMainWorktree has run and the main row exists.
	if err := patchMainWorktreeConfig(ctx, repoRoot, true); err != nil {
		return err
	}
	// Kick off setup hooks + prepare for the freshly-enrolled main row.
	// The reload above is synchronous: by the time it returns, Enroll
	// MainWorktree has run and the row exists, so FinalizeWorktree
	// routes through the main-wt path (slug.ForMain + overlay) and
	// fires create-* hooks with $TREEMAN_IS_MAIN=1.
	if err := requestWorktreeFinalize(ctx, repoRoot, repoRoot); err != nil {
		ui.Warn("daemon finalize dispatch failed: %v", err)
		ui.Hint("run `treeman worktree finalize .` at the repo root to retry")
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
	// Purge runs in the daemon, BEFORE we patch the config: the daemon
	// loads config from disk, so it must see the still-enabled engine +
	// template defs that the purge needs. Streamed foreground so the
	// disable below only fires once the per-branch DROP loop has finished.
	if c.Bool("purge") {
		if err := dispatchTask(ctx, rpc.Task{
			Type: rpc.TaskMainPurgeDBs, RepoPath: repoRoot,
			InheritedEnv: CaptureInheritedEnv(),
		}, true, "main purge"); err != nil {
			ui.Warn("purge: %v", err)
			ui.Hint("config left enabled; re-run `treeman main disable --purge` after fixing the engine reachability issue")
			return nil
		}
	}
	if err := patchMainWorktreeConfig(ctx, repoRoot, false); err != nil {
		return err
	}
	PrintOK("main worktree disabled (%s)", repoRoot)
	return nil
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

	enabled := ui.Gray("false")
	if cfg.MainWorktree.Enabled {
		enabled = ui.Green("true")
	}
	ui.Plain("%s", ui.Bold(repoRoot))
	ui.Plain("  %s %s", ui.Dim("enabled        :"), enabled)
	ui.Plain("  %s %s", ui.Dim("current branch :"), ui.Cyan(branch))
	if row.ID == 0 {
		ui.Plain("  %s %s", ui.Dim("enrolled       :"), ui.Gray("no row"))
		return nil
	}
	ui.Plain("  %s %s %s", ui.Dim("enrolled slug  :"), ui.Cyan(row.Slug), ui.Dim(fmt.Sprintf("(row %d)", row.ID)))
	ui.Plain("  %s %s", ui.Dim("enrolled branch:"), ui.Cyan(row.Branch))
	return nil
}

// patchMainWorktreeConfig surgically rewrites the `main_worktree:`
// block in `.treeman.yaml`, preserving surrounding comments. Falls
// back to creating the file with a minimal `main_worktree:` stanza
// when the repo has no .treeman.yaml yet — a virgin repo wanting to
// opt-in shouldn't need to scaffold a full config first.
func patchMainWorktreeConfig(ctx context.Context, repoRoot string, enabled bool) error {
	target, body, err := renderMainWorktreeConfig(repoRoot, enabled)
	if err != nil {
		return err
	}
	return writeConfig(ctx, repoRoot, target, body)
}

// renderMainWorktreeConfig is the pure render half of
// patchMainWorktreeConfig: it reads .treeman.yaml, sets
// main_worktree.enabled, and returns the (path, new body). No write —
// the daemon performs the persist (see writeConfig).
func renderMainWorktreeConfig(repoRoot string, enabled bool) (string, []byte, error) {
	target := filepath.Join(repoRoot, ".treeman.yaml")
	raw, err := os.ReadFile(target)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("read %s: %w", target, err)
		}
		raw = nil
	}

	var doc yaml.Node
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return "", nil, fmt.Errorf("parse %s: %w", target, err)
		}
	}
	if doc.Kind == 0 {
		// Empty file or missing — seed an empty mapping document the
		// yamlpatch helpers can splice into.
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}

	enabledSegs, err := yamlpatch.ParsePath("main_worktree.enabled")
	if err != nil {
		return "", nil, err
	}
	enabledNode, _ := yamlpatch.ValueToNode(enabled)
	if _, err := yamlpatch.Set(&doc, enabledSegs, enabledNode); err != nil {
		return "", nil, err
	}
	body, err := yamlpatch.Marshal(&doc)
	if err != nil {
		return "", nil, err
	}
	return target, body, nil
}

// requestWorktreeFinalize dispatches a finalize RPC for wtPath under
// repoRoot. Used by `main enable` to kick off create-* hooks +
// prepare against the freshly-enrolled main row without making the
// user manually run `treeman worktree finalize .`. The CLI captures the
// user's env here because the daemon goroutine wouldn't otherwise
// see PATH / nvm shims / etc.
func requestWorktreeFinalize(ctx context.Context, repoRoot, wtPath string) error {
	// submitPlan dispatches to the daemon when reachable, else runs the
	// finalize tail in-process — so `main enable` works daemon-less.
	if resp := submitPlan(ctx, finalizeRequest(repoRoot, wtPath)); resp.Kind == rpc.KindError {
		return fmt.Errorf("finalize: %s", resp.Message)
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
