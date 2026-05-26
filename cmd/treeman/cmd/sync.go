package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/ui"
)

// SyncCmd — `treeman sync [path]` runs an on-demand git fetch +
// advance against the targeted scope (cwd's repo by default, or every
// registered repo with --all). Surfaces the per-repo result so the
// user sees "advanced" vs "skipped (dirty)" without rummaging in
// logs_query.
func SyncCmd() *cli.Command {
	return &cli.Command{
		Name:      "sync",
		Usage:     "fetch + advance worktrees to upstream (manual override of auto-fetch)",
		ArgsUsage: "[path]",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "sync every registered repo"},
			&cli.BoolFlag{Name: "json", Usage: "emit one JSON object per repo"},
			&cli.BoolFlag{Name: "status", Usage: "print sync status only; do not fetch"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			target := ""
			if c.NArg() > 0 {
				target = MustAbs(c.Args().First())
			} else if !c.Bool("all") {
				// Default: scope to cwd's repo if we can find one.
				if root, err := DiscoverRepoRoot(MustAbs(".")); err == nil {
					target = root
				}
			}

			if c.Bool("status") {
				return syncStatusAction(ctx, target, c.Bool("json"))
			}

			resp, err := rpc.Call(ctx, rpc.Request{
				Method:  rpc.MethodSyncNow,
				SyncNow: &rpc.SyncNowArgs{Path: target},
			})
			if err != nil {
				return err
			}
			if resp.Kind == rpc.KindError {
				return fmt.Errorf("daemon: %s", resp.Message)
			}

			if c.Bool("json") {
				return jsonStream(map[string]any{
					"repos":  resp.SyncedRepos,
					"errors": resp.SyncErrors,
				})
			}

			for _, e := range resp.SyncErrors {
				ui.Warn("%s", e)
			}
			renderSyncStatuses(resp.SyncedRepos)
			return nil
		},
	}
}

func syncStatusAction(ctx context.Context, target string, jsonOut bool) error {
	resp, err := rpc.Call(ctx, rpc.Request{
		Method:     rpc.MethodSyncStatus,
		SyncStatus: &rpc.SyncStatusArgs{RepoPath: target},
	})
	if err != nil {
		return err
	}
	if resp.Kind == rpc.KindError {
		return fmt.Errorf("daemon: %s", resp.Message)
	}
	if jsonOut {
		return jsonStream(map[string]any{"repos": resp.SyncedRepos})
	}
	renderSyncStatuses(resp.SyncedRepos)
	return nil
}

func renderSyncStatuses(rows []rpc.SyncRepoStatus) {
	if len(rows) == 0 {
		ui.Hint("no registered repos")
		return
	}
	for _, r := range rows {
		last := "never"
		if r.LastFetchUnix > 0 {
			last = time.Unix(r.LastFetchUnix, 0).Format(time.RFC3339)
		}
		retry := ""
		if r.NextRetryUnix > 0 {
			retry = fmt.Sprintf(" backoff_until=%s (failures=%d)",
				time.Unix(r.NextRetryUnix, 0).Format(time.RFC3339), r.ConsecFailures)
		}
		fmt.Printf("%s  mode=%s last_fetch=%s%s\n", ui.Bold(r.RepoPath), r.Mode, last, retry)
		for _, w := range r.Worktrees {
			tag := ""
			switch {
			case w.LastSkipReason != "":
				tag = " skip=" + w.LastSkipReason
			case w.Ahead == 0 && w.Behind == 0 && !w.Dirty:
				tag = " in-sync"
			}
			fmt.Printf("    %s  branch=%s ahead=%d behind=%d dirty=%v%s\n",
				w.Path, w.Branch, w.Ahead, w.Behind, w.Dirty, tag)
		}
	}
}
