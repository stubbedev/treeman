package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

// wtShow — `treeman worktree show <name>` prints a per-worktree dossier:
// metadata, current finalize state, recent events, recent hook runs.
// Single-page output designed to answer "what's going on with this
// worktree?" without forcing the operator to compose three queries.
func wtShow() *cli.Command {
	return &cli.Command{
		Name:          "show",
		Usage:         "show details, recent events, and hook runs for a worktree (defaults to the worktree containing the current directory)",
		ArgsUsage:     "[worktree]",
		ShellComplete: worktreeArgComplete,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.IntFlag{Name: "events", Value: 10, Usage: "number of recent events to show"},
			&cli.IntFlag{Name: "hooks", Value: 5, Usage: "number of recent hook runs to show"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			pager := newPagerIfEligible(c, false, c.Bool("json"))
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()

			repoID := resolveShowRepoID(ctx, st, c.String("repo"))

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

			// Recent events.
			evs, _ := st.QueryEvents(ctx, store.EventFilter{
				WorktreeID: wt.ID,
				Limit:      c.Int("events"),
				HydrateWT:  false,
			})
			reverseEvents(evs)

			if c.Bool("json") {
				runs, _ := st.QueryHookRuns(ctx, wt.ID, c.Int("hooks"))
				state, detail := finalizeState(ctx, st, wt.ID)
				ports, _ := st.LoadWorktreePorts(ctx, wt.ID)
				branches, _ := st.ListActiveBranches(ctx, wt.ID)
				return jsonStream(map[string]any{
					"id":            wt.ID,
					"slug":          wt.Slug,
					"branch":        wt.Branch,
					"path":          wt.Path,
					"created_at":    wt.CreatedAt,
					"state":         ui.StripANSI(state),
					"state_detail":  ui.StripANSI(detail),
					"ports":         ports,
					"branch_scoped": branches,
					"events":        evs,
					"hook_runs":     runs,
				})
			}

			printWtHeader(ctx, st, wt)
			printWtPrepareSummary(ctx, st, wt.ID)
			if len(evs) > 0 {
				_, _ = fmt.Fprintln(ui.Out, ui.Bold("recent events"))
				for _, e := range evs {
					printEvent(false, e)
				}
				_, _ = fmt.Fprintln(ui.Out)
			}

			// Recent hook runs.
			runs, _ := st.QueryHookRuns(ctx, wt.ID, c.Int("hooks"))
			printWtHookRuns(runs)

			ui.Hint("follow live events: treeman worktree logs %s --follow", wt.Slug)
			return nil
		},
	}
}

// resolveShowRepoID resolves the repo id to scope `wt show` lookups to:
// the explicit --repo override, else the repo containing cwd. Returns 0
// when neither resolves (an unscoped lookup).
func resolveShowRepoID(ctx context.Context, st *store.Store, repoOverride string) int64 {
	repoID := int64(0)
	if repoOverride != "" {
		repoID, _ = lookupRepoID(ctx, st, MustAbs(repoOverride))
	} else if cwd, err := os.Getwd(); err == nil {
		if root, err := DiscoverRepoRoot(cwd); err == nil {
			repoID, _ = lookupRepoID(ctx, st, root)
		}
	}
	return repoID
}

// printWtHeader prints the metadata block (slug, branch, path, state,
// ports, branch-scoped DBs) for a worktree.
func printWtHeader(ctx context.Context, st *store.Store, wt worktreeRow) {
	_, _ = fmt.Fprintln(ui.Out, ui.Bold(wt.Slug)+ui.Dim("  #"+strconv.FormatInt(wt.ID, 10)))
	_, _ = fmt.Fprintf(ui.Out, "  branch: %s\n", wt.Branch)
	_, _ = fmt.Fprintf(ui.Out, "  path:   %s\n", wt.Path)
	_, _ = fmt.Fprintf(ui.Out, "  state:  %s\n", finalizeStatusLine(ctx, st, wt.ID))
	if portMap, _ := st.LoadWorktreePorts(ctx, wt.ID); len(portMap) > 0 {
		_, _ = fmt.Fprintf(ui.Out, "  ports:  %s\n", formatPortMap(portMap))
	}
	if bs, _ := st.ListActiveBranches(ctx, wt.ID); len(bs) > 0 {
		for _, r := range bs {
			_, _ = fmt.Fprintf(ui.Out, "  branch_scoped: %s (%s) → active branch %s\n", r.DBKey, r.Engine, r.Branch)
		}
	}
	_, _ = fmt.Fprintln(ui.Out)
}

// printWtPrepareSummary prints the "last prepare" block: per-database
// outcome (cache hit vs cold build + wall-clock + clone count) and the
// per-phase duration breakdown from that run's prepare:phase events.
// Everything here was already in the event log — this renders it so
// "why was that create slow?" doesn't require spelunking `logs tail`.
func printWtPrepareSummary(ctx context.Context, st *store.Store, wtID int64) {
	ends, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtPrepareEnd},
		Limit:      10,
	})
	if len(ends) == 0 {
		return
	}
	// Newest run per (engine, source_db) — the query returns newest
	// first, so first sighting wins.
	type prep struct {
		engine, sourceDB, cacheHit, clones string
		durMs, ts                          int64
	}
	seen := map[string]bool{}
	var preps []prep
	oldest := ends[0].Ts
	for _, e := range ends {
		p := decodePayload(e.PayloadJSON)
		key := p["engine"] + "|" + p["source_db"]
		if seen[key] {
			continue
		}
		seen[key] = true
		dur := e.DurationMs.Int64
		if ms, err := strconv.ParseInt(p["duration_ms"], 10, 64); err == nil {
			dur = ms
		}
		preps = append(preps, prep{
			engine: p["engine"], sourceDB: p["source_db"],
			cacheHit: p["cache_hit"], clones: p["clones"],
			durMs: dur, ts: e.Ts,
		})
		if e.Ts < oldest {
			oldest = e.Ts
		}
	}
	// Phase breakdowns belong to the runs above: scope to a window just
	// before the oldest rendered prepare:end (phases precede their end
	// event; 30 minutes generously covers the slowest cold build).
	phases, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID:  wtID,
		EventTypes:  []string{store.EvtPreparePhase},
		SinceMs:     oldest - 30*60*1000,
		OldestFirst: true,
		Limit:       100,
	})
	phaseByKey := map[string][]string{}
	for _, e := range phases {
		p := decodePayload(e.PayloadJSON)
		key := p["engine"] + "|" + p["source_db"]
		phaseByKey[key] = append(phaseByKey[key],
			fmt.Sprintf("%s %s", e.Phase, (time.Duration(e.DurationMs.Int64)*time.Millisecond).String()))
	}

	_, _ = fmt.Fprintln(ui.Out, ui.Bold("last prepare"))
	for _, p := range preps {
		mode := ui.Yellow("cold build")
		if p.cacheHit == "true" {
			mode = ui.Green("cache hit")
		}
		line := fmt.Sprintf("  %s %s: %s in %s", ui.Cyan(p.engine), p.sourceDB, mode,
			(time.Duration(p.durMs) * time.Millisecond).String())
		if p.clones != "" && p.clones != "0" {
			line += fmt.Sprintf(" (%s clones)", p.clones)
		}
		line += ui.Dim("  " + formatTs(p.ts))
		_, _ = fmt.Fprintln(ui.Out, line)
		if ph := phaseByKey[p.engine+"|"+p.sourceDB]; len(ph) > 0 {
			_, _ = fmt.Fprintln(ui.Out, ui.Dim("    "+strings.Join(ph, " · ")))
		}
	}
	_, _ = fmt.Fprintln(ui.Out)
}

// decodePayload unmarshals an event's payload_json into a flat string
// map; malformed or empty payloads yield an empty map.
func decodePayload(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// printWtHookRuns prints the recent hook runs table (nothing when the
// slice is empty).
func printWtHookRuns(runs []store.HookRun) {
	if len(runs) == 0 {
		return
	}
	_, _ = fmt.Fprintln(ui.Out, ui.Bold("recent hook runs"))
	tbl := ui.NewTable("STARTED", "PHASE", "EXIT", "DURATION")
	for _, h := range runs {
		exit := ui.Yellow("running")
		if h.ExitCode.Valid {
			s := strconv.FormatInt(h.ExitCode.Int64, 10)
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
	_, _ = fmt.Fprintln(ui.Out)
}

// wtLogs — `treeman worktree logs <name>` is a convenience wrapper around
// `treeman logs tail --worktree <name>` so users don't need to know
// the filter exists.
func wtLogs() *cli.Command {
	return &cli.Command{
		Name:          "logs",
		Usage:         "tail events scoped to a worktree (shorthand for `logs tail --worktree`)",
		ArgsUsage:     "<worktree>",
		ShellComplete: worktreeArgComplete,
		Flags: append(filterFlags(),
			&cli.BoolFlag{Name: "hooks", Usage: "show recent hook_runs rows instead of events"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman worktree logs <worktree>")
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
				argv = append(argv, "--n", strconv.Itoa(n))
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

// wtWait — `treeman worktree wait <name>` blocks until the most-recent
// daemon-detached finalize for that worktree has either succeeded
// (`worktree:create:end`) or failed (level=error, event_type=worktree:create:error).
// Exit code reflects the outcome — 0 on success, non-zero on failure
// or timeout — so CI scripts can `treeman worktree create FOO && treeman
// wt wait FOO`.
func wtWait() *cli.Command {
	return &cli.Command{
		Name:          "wait",
		Usage:         "block until the daemon's finalize for a worktree completes",
		ArgsUsage:     "<worktree>",
		ShellComplete: worktreeArgComplete,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "give up after this duration"},
			&cli.BoolFlag{Name: "quiet", Aliases: []string{"q"}, Usage: "suppress progress output (still exits non-zero on failure)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errors.New("usage: treeman worktree wait <worktree>")
			}
			name := c.Args().First()
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()
			repoID := resolveShowRepoID(ctx, st, c.String("repo"))
			wt, err := loadWorktreeRow(ctx, st, repoID, name)
			if err != nil {
				return err
			}

			// Anchor the wait at the newest worktree:create:start we can
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

			return pollFinalize(ctx, st, wt, anchor, deadline, c.Duration("timeout"), c.Bool("quiet"))
		},
	}
}

// pollFinalize polls the event log every 500ms until the worktree's
// finalize completes (success → nil), fails (error event → error) or the
// deadline passes (timeout → error).
func pollFinalize(
	ctx context.Context,
	st *store.Store,
	wt worktreeRow,
	anchor int64,
	deadline time.Time,
	timeout time.Duration,
	quiet bool,
) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		rows, err := st.QueryEvents(ctx, store.EventFilter{
			WorktreeID:  wt.ID,
			EventTypes:  []string{store.EvtWorktreeCreateEnd, store.EvtWorktreeCreateError},
			SinceMs:     anchor,
			OldestFirst: true,
			Limit:       50,
		})
		if err != nil {
			return err
		}
		for _, e := range rows {
			if e.EventType == store.EvtWorktreeCreateEnd {
				if !quiet {
					ui.Success("finalize complete for %s", wt.Slug)
				}
				return nil
			}
			if e.EventType == store.EvtWorktreeCreateError && e.Level == store.LevelError {
				return fmt.Errorf("finalize failed: %s", e.Message)
			}
		}
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
// worktree row. Lets `treeman worktree show` (and friends) run argument-
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
	return worktreeRow{}, errors.New(
		"not inside a tracked worktree (run from inside one, or pass a worktree name — see `treeman worktree list`)",
	)
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
	id, err := st.LookupWorktreeID(ctx, repoID, name)
	if err != nil {
		return wt, err
	}
	if id == 0 {
		return wt, fmt.Errorf("no worktree matches %q (try `treeman worktree list`)", name)
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
		EventTypes: []string{
			store.EvtWorktreeCreateStart,
			store.EvtWorktreeCreateEnd,
			store.EvtWorktreeCreateError,
			store.EvtWorktreeCreateDeferred,
		},
		Limit: 1,
	})
	if len(rows) == 0 {
		return ui.Status("ready"), ""
	}
	last := rows[0]
	switch last.EventType {
	case store.EvtWorktreeCreateEnd:
		return ui.Status("ready"), "(last finalize " + formatTs(last.Ts) + ")"
	case store.EvtWorktreeCreateStart:
		return ui.Status("preparing"), "(started " + formatTs(last.Ts) + ")"
	case store.EvtWorktreeCreateDeferred:
		return ui.Status("deferred"), "— " + last.Message
	case store.EvtWorktreeCreateError:
		if last.Level == store.LevelError {
			return ui.Status("error"), "— " + last.Message
		}
	}
	return ui.Status("ready"), ""
}

func newestStartTs(ctx context.Context, st *store.Store, wtID int64) int64 {
	rows, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{store.EvtWorktreeCreateStart},
		Limit:      1,
	})
	if len(rows) == 0 {
		return 0
	}
	return rows[0].Ts
}
