package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

// wtShow — `treeman wt show <name>` prints a per-worktree dossier:
// metadata, current finalize state, recent events, recent hook runs.
// Single-page output designed to answer "what's going on with this
// worktree?" without forcing the operator to compose three queries.
func wtShow() *cli.Command {
	return &cli.Command{
		Name:      "show",
		Aliases:   []string{"info"},
		Usage:     "show details, recent events, and hook runs for a worktree (defaults to the worktree containing the current directory)",
		ArgsUsage: "[worktree]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.IntFlag{Name: "events", Value: 10, Usage: "number of recent events to show"},
			&cli.IntFlag{Name: "hooks", Value: 5, Usage: "number of recent hook runs to show"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			pager := newPagerIfEligible(c, false, false)
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()

			repoID := int64(0)
			if r := c.String("repo"); r != "" {
				repoID, _ = lookupRepoID(ctx, st, MustAbs(r))
			} else if cwd, err := os.Getwd(); err == nil {
				if root, err := DiscoverRepoRoot(cwd); err == nil {
					repoID, _ = lookupRepoID(ctx, st, root)
				}
			}

			var wt worktreeRow
			if c.NArg() >= 1 {
				wt, err = loadWorktreeRow(ctx, st, repoID, c.Args().First())
			} else {
				// No argument — resolve the worktree containing cwd.
				wt, err = worktreeFromCwd(ctx, st)
			}
			if err != nil {
				return err
			}

			fmt.Fprintln(ui.Out, ui.Bold(wt.Slug)+ui.Dim("  #"+fmt.Sprintf("%d", wt.ID)))
			fmt.Fprintf(ui.Out, "  branch: %s\n", wt.Branch)
			fmt.Fprintf(ui.Out, "  path:   %s\n", wt.Path)
			fmt.Fprintf(ui.Out, "  state:  %s\n", finalizeStatusLine(ctx, st, wt.ID))
			if portMap, _ := st.LoadWorktreePorts(ctx, wt.ID); len(portMap) > 0 {
				fmt.Fprintf(ui.Out, "  ports:  %s\n", formatPortMap(portMap))
			}
			if bs, _ := st.ListActiveBranches(ctx, wt.ID); len(bs) > 0 {
				for _, r := range bs {
					fmt.Fprintf(ui.Out, "  branch_scoped: %s (%s) → active branch %s\n", r.DBKey, r.Engine, r.Branch)
				}
			}
			fmt.Fprintln(ui.Out)

			// Recent events.
			evs, _ := st.QueryEvents(ctx, store.EventFilter{
				WorktreeID: wt.ID,
				Limit:      int(c.Int("events")),
				HydrateWT:  false,
			})
			reverseEvents(evs)
			if len(evs) > 0 {
				fmt.Fprintln(ui.Out, ui.Bold("recent events"))
				for _, e := range evs {
					printEvent(false, e)
				}
				fmt.Fprintln(ui.Out)
			}

			// Recent hook runs.
			runs, _ := st.QueryHookRuns(ctx, wt.ID, int(c.Int("hooks")))
			if len(runs) > 0 {
				fmt.Fprintln(ui.Out, ui.Bold("recent hook runs"))
				tbl := ui.NewTable("STARTED", "PHASE", "EXIT", "DURATION")
				for _, h := range runs {
					exit := ui.Yellow("running")
					if h.ExitCode.Valid {
						s := fmt.Sprintf("%d", h.ExitCode.Int64)
						if h.ExitCode.Int64 == 0 {
							exit = ui.Green(s)
						} else {
							exit = ui.Red(s)
						}
					}
					dur := "—"
					if h.FinishedAt.Valid {
						dur = ui.Dim((time.Duration(h.FinishedAt.Int64-h.StartedAt) * time.Millisecond).String())
					}
					tbl.Row(ui.Dim(formatTs(h.StartedAt)), ui.Cyan(h.Phase), exit, dur)
				}
				tbl.Render(nil)
				fmt.Fprintln(ui.Out)
			}

			ui.Hint("follow live events: treeman wt logs %s --follow", wt.Slug)
			return nil
		},
	}
}

// wtLogs — `treeman wt logs <name>` is a convenience wrapper around
// `treeman logs tail --worktree <name>` so users don't need to know
// the filter exists.
func wtLogs() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "tail events scoped to a worktree (shorthand for `logs tail --worktree`)",
		ArgsUsage: "<worktree>",
		Flags: append(filterFlags(),
			&cli.BoolFlag{Name: "hooks", Usage: "show recent hook_runs rows instead of events"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt logs <worktree>")
			}
			name := c.Args().First()
			// Delegate to the logs command's argv shape so the
			// filter surface stays single-source.
			if c.Bool("hooks") {
				argv := []string{"logs", "hooks", name}
				if r := c.String("repo"); r != "" {
					argv = append(argv, "--repo", r)
				}
				if c.Bool("json") {
					argv = append(argv, "--json")
				}
				return LogsCmd().Run(ctx, argv)
			}
			argv := []string{"logs", "tail", "--worktree", name}
			if c.Bool("follow") {
				argv = append(argv, "--follow")
			}
			if c.Bool("json") {
				argv = append(argv, "--json")
			}
			if c.Bool("no-pager") {
				argv = append(argv, "--no-pager")
			}
			if n := c.Int("n"); n > 0 {
				argv = append(argv, "--n", fmt.Sprintf("%d", n))
			}
			if r := c.String("repo"); r != "" {
				argv = append(argv, "--repo", r)
			}
			for _, l := range c.StringSlice("level") {
				argv = append(argv, "--level", l)
			}
			for _, p := range c.StringSlice("phase") {
				argv = append(argv, "--phase", p)
			}
			for _, et := range c.StringSlice("event-type") {
				argv = append(argv, "--event-type", et)
			}
			if s := c.String("since"); s != "" {
				argv = append(argv, "--since", s)
			}
			return LogsCmd().Run(ctx, argv)
		},
	}
}

// wtWait — `treeman wt wait <name>` blocks until the most-recent
// daemon-detached finalize for that worktree has either succeeded
// (`wt_finalize_done`) or failed (level=error, event_type=wt_finalize).
// Exit code reflects the outcome — 0 on success, non-zero on failure
// or timeout — so CI scripts can `treeman wt create FOO && treeman
// wt wait FOO`.
func wtWait() *cli.Command {
	return &cli.Command{
		Name:      "wait",
		Usage:     "block until the daemon's finalize for a worktree completes",
		ArgsUsage: "<worktree>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "give up after this duration"},
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "suppress progress output (still exits non-zero on failure)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman wt wait <worktree>")
			}
			name := c.Args().First()
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()
			repoID := int64(0)
			if r := c.String("repo"); r != "" {
				repoID, _ = lookupRepoID(ctx, st, MustAbs(r))
			} else if cwd, err := os.Getwd(); err == nil {
				if root, err := DiscoverRepoRoot(cwd); err == nil {
					repoID, _ = lookupRepoID(ctx, st, root)
				}
			}
			wt, err := loadWorktreeRow(ctx, st, repoID, name)
			if err != nil {
				return err
			}

			// Anchor the wait at the newest wt_finalize_start we can
			// see. If none exists yet, fall back to the wt's creation
			// time so we don't terminate prematurely against a
			// historical "done" row.
			anchor := newestStartTs(ctx, st, wt.ID)
			if anchor == 0 {
				anchor = wt.CreatedAt
			}
			deadline := time.Now().Add(c.Duration("timeout"))
			if !c.Bool("quiet") {
				ui.Info("waiting for finalize on %s (timeout %s)…", ui.Cyan(wt.Slug), c.Duration("timeout"))
			}

			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				if time.Now().After(deadline) {
					return fmt.Errorf("timed out after %s", c.Duration("timeout"))
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
				}
				rows, err := st.QueryEvents(ctx, store.EventFilter{
					WorktreeID:  wt.ID,
					EventTypes:  []string{"wt_finalize_done", "wt_finalize"},
					SinceMs:     anchor,
					OldestFirst: true,
					Limit:       50,
				})
				if err != nil {
					return err
				}
				for _, e := range rows {
					if e.EventType == "wt_finalize_done" {
						if !c.Bool("quiet") {
							ui.Success("finalize complete for %s", wt.Slug)
						}
						return nil
					}
					if e.EventType == "wt_finalize" && e.Level == "error" {
						return fmt.Errorf("finalize failed: %s", e.Message)
					}
				}
			}
		},
	}
}

// worktreeRow is a slim record returned by loadWorktreeRow.
type worktreeRow struct {
	ID        int64
	Slug      string
	Branch    string
	Path      string
	CreatedAt int64
}

// worktreeFromCwd resolves the worktree row whose path contains the
// current working directory. Walks parent directories from cwd up to
// the filesystem root, returning the first that matches an active
// worktree row. Lets `treeman wt show` (and friends) run argument-
// free from anywhere inside a worktree.
func worktreeFromCwd(ctx context.Context, st *store.Store) (worktreeRow, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return worktreeRow{}, err
	}
	dir := MustAbs(cwd)
	for {
		row, err := st.LookupActiveWorktreeByPath(ctx, dir)
		if err != nil {
			return worktreeRow{}, err
		}
		if row.ID != 0 {
			return worktreeRow{
				ID:     row.ID,
				Slug:   row.Slug,
				Branch: row.Branch,
				Path:   row.Path,
			}, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return worktreeRow{}, fmt.Errorf("not inside a tracked worktree (run from inside one, or pass a worktree name — see `treeman wt list`)")
}

// formatPortMap renders a slot→port map as "name=port name=port" in
// stable alphabetical order.
func formatPortMap(ports map[string]uint16) string {
	names := store.SortedSlotNames(ports)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, ports[n]))
	}
	return strings.Join(parts, " ")
}

func loadWorktreeRow(ctx context.Context, st *store.Store, repoID int64, name string) (worktreeRow, error) {
	var wt worktreeRow
	id, _ := st.LookupWorktreeID(ctx, repoID, name)
	if id == 0 {
		return wt, fmt.Errorf("no worktree matches %q (try `treeman wt list`)", name)
	}
	row := st.DB.QueryRowContext(ctx, `SELECT id, slug, COALESCE(branch,''), path, created_at
		FROM worktrees WHERE id = ?`, id)
	if err := row.Scan(&wt.ID, &wt.Slug, &wt.Branch, &wt.Path, &wt.CreatedAt); err != nil {
		return wt, err
	}
	return wt, nil
}

// finalizeStatusLine derives a human "state" label for `wt show` by
// inspecting the most recent finalize-related event. Returns the
// long form (state + parenthetical timestamp / error message).
func finalizeStatusLine(ctx context.Context, st *store.Store, wtID int64) string {
	state, detail := finalizeState(ctx, st, wtID)
	if detail == "" {
		return state
	}
	return state + ui.Dim(" "+detail)
}

// finalizeStateShort returns just the colored state token, for use
// in tables where a long parenthetical would overflow the column.
func finalizeStateShort(ctx context.Context, st *store.Store, wtID int64) string {
	state, _ := finalizeState(ctx, st, wtID)
	return state
}

// finalizeState splits the state derivation so both forms share one
// query path. detail is "" when there's nothing extra worth showing.
func finalizeState(ctx context.Context, st *store.Store, wtID int64) (state, detail string) {
	rows, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{"wt_finalize_start", "wt_finalize_done", "wt_finalize"},
		Limit:      1,
	})
	if len(rows) == 0 {
		return ui.Status("ready"), ""
	}
	last := rows[0]
	switch last.EventType {
	case "wt_finalize_done":
		return ui.Status("ready"), "(last finalize " + formatTs(last.Ts) + ")"
	case "wt_finalize_start":
		return ui.Status("preparing"), "(started " + formatTs(last.Ts) + ")"
	case "wt_finalize":
		if last.Level == "error" {
			return ui.Status("error"), "— " + last.Message
		}
	}
	return ui.Status("ready"), ""
}

func newestStartTs(ctx context.Context, st *store.Store, wtID int64) int64 {
	rows, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{"wt_finalize_start"},
		Limit:      1,
	})
	if len(rows) == 0 {
		return 0
	}
	return rows[0].Ts
}
