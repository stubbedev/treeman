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
	PrintOK("main worktree enabled (%s)", repoRoot)
	return nil
}

func mainDisableAction(ctx context.Context, c *cli.Command) error {
	repoRoot, err := resolveRepo(c.String("repo"))
	if err != nil {
		return err
	}
	if err := patchMainWorktreeConfig(repoRoot, false); err != nil {
		return err
	}
	if err := requestConfigReload(ctx, repoRoot); err != nil {
		ui.Warn("daemon reload failed: %v", err)
		ui.Hint("config written; restart treemand to apply")
		return nil
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
	defer st.Close()
	repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil || repoID == 0 {
		return store.WorktreeRow{}, branch
	}
	row, _ := st.LookupMainWorktree(ctx, repoID)
	return row, branch
}
