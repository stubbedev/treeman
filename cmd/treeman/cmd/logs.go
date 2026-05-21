package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
)

// LogsCmd — `treeman logs {tail,grep,hooks}` queries the SQLite event
// log. Every read subcommand shares the same filter surface so
// `--worktree foo --level warn` works uniformly across `tail` and
// `grep`.
func LogsCmd() *cli.Command {
	return &cli.Command{
		Name:    "logs",
		Aliases: []string{"log"},
		Usage:   "query the SQLite event log",
		Commands: []*cli.Command{
			logsTail(),
			logsGrep(),
			logsHooks(),
		},
	}
}

// filterFlags returns the flag set shared by tail + grep. Kept as
// pointers so the surface stays consistent.
func filterFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{Name: "n", Value: 50, Usage: "max events to return"},
		&cli.BoolFlag{Name: "follow", Aliases: []string{"f"}, Usage: "stream new events as they arrive"},
		&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}, Usage: "filter by worktree (slug, branch, or basename)"},
		&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo root override"},
		&cli.StringSliceFlag{Name: "level", Aliases: []string{"l"}, Usage: "filter by level (debug|info|warn|error); repeatable"},
		&cli.StringSliceFlag{Name: "event-type", Aliases: []string{"t"}, Usage: "filter by exact event_type; repeatable"},
		&cli.StringSliceFlag{Name: "phase", Aliases: []string{"p"}, Usage: "filter by phase (precreate|postcreate|...); repeatable"},
		&cli.StringFlag{Name: "since", Usage: "only events newer than this (e.g. 10m, 2h, 2026-05-21T00:00)"},
		&cli.BoolFlag{Name: "json", Usage: "emit one JSON object per line instead of the formatted columns"},
		&cli.StringFlag{Name: "payload", Usage: "substring match against payload_json"},
	}
}

func logsTail() *cli.Command {
	return &cli.Command{
		Name:  "tail",
		Usage: "print recent events, optionally streaming new ones with --follow",
		Description: `Examples:
  treeman logs tail
  treeman logs tail --follow
  treeman logs tail --worktree PROJ-1234 --level warn --level error
  treeman logs tail --since 5m --json | jq .
  treeman logs tail --event-type wt_finalize_done --event-type wt_finalize_start

When stdout is a terminal and --follow / --json are not used,
output is paged through $PAGER (default: less -FRX). Set
TREEMAN_NO_PAGER=1 to disable.`,
		Flags: append(filterFlags(),
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			f, err := buildFilter(ctx, c)
			if err != nil {
				return err
			}
			f.Limit = int(c.Int("n"))
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()
			rows, err := st.QueryEvents(ctx, f)
			if err != nil {
				return err
			}
			// Reverse so oldest-first on screen (matches what the
			// caller's eye expects when tail-following).
			reverseEvents(rows)
			follow := c.Bool("follow")
			asJSON := c.Bool("json")
			pager := newPagerIfEligible(c, follow, asJSON)
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			lastID := int64(0)
			for _, e := range rows {
				printEvent(asJSON, e)
				if e.ID > lastID {
					lastID = e.ID
				}
			}
			if !follow {
				return nil
			}
			return followLoop(ctx, st, f, lastID, asJSON)
		},
	}
}

func logsGrep() *cli.Command {
	return &cli.Command{
		Name:      "grep",
		Usage:     "search events whose message (or --payload) matches a pattern",
		ArgsUsage: "<pattern>",
		Description: `Examples:
  treeman logs grep "snapshot cache"
  treeman logs grep "^prepare" --regex
  treeman logs grep checksum --search-payload --level info`,
		Flags: append(filterFlags(),
			&cli.BoolFlag{Name: "regex", Aliases: []string{"e"}, Usage: "treat the pattern as a Go regexp instead of a substring"},
			&cli.BoolFlag{Name: "search-payload", Usage: "search the payload_json column instead of the message"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman logs grep <pattern>")
			}
			pattern := c.Args().First()
			f, err := buildFilter(ctx, c)
			if err != nil {
				return err
			}
			if c.Int("n") > 0 {
				f.Limit = int(c.Int("n"))
			} else {
				f.Limit = 500
			}
			useRegex := c.Bool("regex")
			searchPayload := c.Bool("search-payload")
			var re *regexp.Regexp
			if useRegex {
				re, err = regexp.Compile(pattern)
				if err != nil {
					return fmt.Errorf("invalid regex: %w", err)
				}
			} else {
				// Push the substring into SQL so we don't drag the
				// whole event table over the wire.
				if searchPayload {
					f.PayloadLike = pattern
				} else {
					f.MessageLike = pattern
				}
			}
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()
			rows, err := st.QueryEvents(ctx, f)
			if err != nil {
				return err
			}
			reverseEvents(rows)
			asJSON := c.Bool("json")
			pager := newPagerIfEligible(c, false, asJSON)
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			matched := 0
			for _, e := range rows {
				if re != nil {
					hay := e.Message
					if searchPayload {
						hay = e.PayloadJSON
					}
					if !re.MatchString(hay) {
						continue
					}
				}
				printEvent(asJSON, e)
				matched++
			}
			if matched == 0 {
				ui.Info("no events matched %q", pattern)
			}
			return nil
		},
	}
}

// newPagerIfEligible returns a ui.Pager when paging makes sense, or
// nil to render inline. Paging is skipped when:
//
//   - --follow is in play (the pager would never see EOF).
//   - --json is set (machine consumers want untouched bytes).
//   - --no-pager is set on this command.
//   - stdout is not a TTY (handled inside ui.NewPager).
func newPagerIfEligible(c *cli.Command, follow, asJSON bool) *ui.Pager {
	if follow || asJSON {
		return nil
	}
	if c != nil && c.Bool("no-pager") {
		return nil
	}
	return ui.NewPager()
}

func logsHooks() *cli.Command {
	return &cli.Command{
		Name:      "hooks",
		Usage:     "show recent hook_runs (precreate/postcreate/predelete) for a worktree",
		ArgsUsage: "<worktree>",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "n", Value: 20, Usage: "max rows"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo root override"},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return fmt.Errorf("usage: treeman logs hooks <worktree>")
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
			wtID, _ := st.LookupWorktreeID(ctx, repoID, name)
			if wtID == 0 {
				return fmt.Errorf("no worktree matches %q", name)
			}
			runs, err := st.QueryHookRuns(ctx, wtID, int(c.Int("n")))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(runs)
			}
			if len(runs) == 0 {
				ui.Info("no hook runs recorded for %s", name)
				return nil
			}
			tbl := ui.NewTable("STARTED", "PHASE", "EXIT", "DURATION", "STDERR_TAIL")
			for _, h := range runs {
				exit := "—"
				if h.ExitCode.Valid {
					code := fmt.Sprintf("%d", h.ExitCode.Int64)
					if h.ExitCode.Int64 == 0 {
						exit = ui.Green(code)
					} else {
						exit = ui.Red(code)
					}
				} else {
					exit = ui.Yellow("running")
				}
				dur := "—"
				if h.FinishedAt.Valid {
					d := time.Duration(h.FinishedAt.Int64-h.StartedAt) * time.Millisecond
					dur = ui.Dim(d.String())
				}
				tail := h.StderrTail
				if tail == "" {
					tail = h.StdoutTail
				}
				tail = singleLine(tail, 80)
				tbl.Row(ui.Dim(formatTs(h.StartedAt)), ui.Cyan(h.Phase), exit, dur, tail)
			}
			tbl.Render(nil)
			return nil
		},
	}
}

// buildFilter assembles an EventFilter from the urfave/cli flag set.
// Worktree + repo flags resolve to integer IDs via SQLite lookups so
// the downstream query is index-friendly.
func buildFilter(ctx context.Context, c *cli.Command) (store.EventFilter, error) {
	f := store.EventFilter{
		Levels:      validateLevels(c.StringSlice("level")),
		EventTypes:  c.StringSlice("event-type"),
		Phases:      c.StringSlice("phase"),
		PayloadLike: c.String("payload"),
		HydrateWT:   true,
	}
	if s := c.String("since"); s != "" {
		t, err := parseSince(s)
		if err != nil {
			return f, err
		}
		f.SinceMs = t.UnixMilli()
	}
	if r := c.String("repo"); r != "" || c.String("worktree") != "" {
		st, closer, err := openLogStore(ctx)
		if err != nil {
			return f, err
		}
		defer closer()
		repoRoot := c.String("repo")
		if repoRoot == "" {
			cwd, _ := os.Getwd()
			repoRoot, _ = DiscoverRepoRoot(cwd)
		}
		if repoRoot != "" {
			f.RepoID, _ = lookupRepoID(ctx, st, MustAbs(repoRoot))
		}
		if w := c.String("worktree"); w != "" {
			id, _ := st.LookupWorktreeID(ctx, f.RepoID, w)
			if id == 0 {
				return f, fmt.Errorf("no worktree matches %q (try `treeman wt list`)", w)
			}
			f.WorktreeID = id
		}
	}
	return f, nil
}

// validateLevels normalises and rejects unknown level tokens early
// rather than letting the SQL silently return zero rows.
func validateLevels(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	allowed := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(v)
		if allowed[v] {
			out = append(out, v)
		}
	}
	return out
}

// parseSince accepts either a duration (`10m`, `2h`) or an absolute
// RFC3339-ish timestamp (`2026-05-21T00:00:00Z`, `2026-05-21`).
func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	formats := []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised --since value %q (try 10m, 2h, 2026-05-21, or RFC3339)", s)
}

func openLogStore(ctx context.Context) (*store.Store, func(), error) {
	p, err := store.DefaultDBPath()
	if err != nil {
		return nil, func() {}, err
	}
	st, err := store.Open(ctx, p)
	if err != nil {
		return nil, func() {}, err
	}
	return st, func() { st.Close() }, nil
}

func lookupRepoID(ctx context.Context, st *store.Store, root string) (int64, error) {
	var id int64
	row := st.DB.QueryRowContext(ctx, `SELECT id FROM repos WHERE path = ?`, root)
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func reverseEvents(in []store.Event) {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
}

// printEvent renders one row. JSON mode emits a flat, line-oriented
// object suitable for `jq` consumption; the human mode produces a
// colored, padded line with worktree context when available.
func printEvent(asJSON bool, e store.Event) {
	if asJSON {
		_ = jsonStream([]store.Event{e})
		return
	}
	level := ui.Level(e.Level)
	et := ui.EventType(e.EventType)
	ts := ui.Dim(formatTs(e.Ts))
	wt := ""
	if e.WorktreeSlug != "" {
		wt = " " + ui.Magenta("["+e.WorktreeSlug+"]")
	}
	phase := ""
	if e.Phase != "" {
		phase = " " + ui.Cyan(e.Phase)
	}
	dur := ""
	if e.DurationMs.Valid && e.DurationMs.Int64 > 0 {
		dur = " " + ui.Dim(fmt.Sprintf("(%dms)", e.DurationMs.Int64))
	}
	msg := e.Message
	fmt.Fprintf(ui.Out, "%s %s %s%s%s %s%s\n", ts, padRight(level, 5), padRight(et, 24), wt, phase, msg, dur)
}

// jsonStream marshals each row as one JSON line.
func jsonStream(v any) error {
	enc := json.NewEncoder(ui.Out)
	switch x := v.(type) {
	case []store.Event:
		for _, e := range x {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	case []store.HookRun:
		for _, h := range x {
			if err := enc.Encode(h); err != nil {
				return err
			}
		}
	default:
		return enc.Encode(v)
	}
	return nil
}

// followLoop polls SQLite for events newer than lastID at a 250ms
// cadence. SQLite WAL means a separate writer (the daemon) doesn't
// block our reads.
func followLoop(ctx context.Context, st *store.Store, baseFilter store.EventFilter, lastID int64, asJSON bool) error {
	baseFilter.OldestFirst = true
	baseFilter.Limit = 0
	baseFilter.AfterID = lastID
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			rows, err := st.QueryEvents(ctx, baseFilter)
			if err != nil {
				return err
			}
			for _, e := range rows {
				printEvent(asJSON, e)
				if e.ID > baseFilter.AfterID {
					baseFilter.AfterID = e.ID
				}
			}
		}
	}
}

// singleLine collapses newlines + trims to width chars for table
// display of stderr_tail / stdout_tail.
func singleLine(s string, width int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > width {
		s = s[:width-1] + "…"
	}
	return strings.TrimSpace(s)
}

