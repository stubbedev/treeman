package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/tui"
	"github.com/stubbedev/treeman/internal/ui"
)

// LogsCmd — `treeman logs {tail,grep,hooks}` queries the SQLite event
// log. Every read subcommand shares the same filter surface so
// `--worktree foo --level warn` works uniformly across `tail` and
// `grep`.
func LogsCmd() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "query the SQLite event log",
		Commands: []*cli.Command{
			logsTail(),
			logsGrep(),
			logsHooks(),
			logsPurge(),
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
		&cli.BoolFlag{Name: "all", Aliases: []string{"A"}, Usage: "show events from every worktree (defeats the cwd auto-filter)"},
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
  treeman logs tail --event-type worktree:create:end --event-type worktree:create:start

When stdout is a terminal and --follow / --json are not used,
output is paged through $PAGER (default: less -FRX). Set
TREEMAN_NO_PAGER=1 to disable.`,
		Flags: append(filterFlags(),
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		),
		Action: func(ctx context.Context, c *cli.Command) error {
			scope, err := buildFilterWithScope(ctx, c)
			if err != nil {
				return err
			}
			f := scope.Filter
			f.Limit = c.Int("n")
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
			style := eventStyle{
				asJSON:  asJSON,
				verbose: c.Bool("verbose") || os.Getenv("TREEMAN_LOGS_VERBOSE") == "1",
				fullTS:  c.Bool("full-ts"),
			}
			printScopePreamble(scope, asJSON)
			pager := newPagerIfEligible(c, follow, asJSON)
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			lastID := int64(0)
			for _, e := range rows {
				printEventStyled(style, e)
				if e.ID > lastID {
					lastID = e.ID
				}
			}
			if !follow {
				return nil
			}
			return followLoop(ctx, st, f, lastID, style)
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
				return errors.New("usage: treeman logs grep <pattern>")
			}
			pattern := c.Args().First()
			scope, err := buildFilterWithScope(ctx, c)
			if err != nil {
				return err
			}
			f := scope.Filter
			if c.Int("n") > 0 {
				f.Limit = c.Int("n")
			} else {
				f.Limit = 500
			}
			useRegex := c.Bool("regex")
			searchPayload := c.Bool("search-payload")
			var re *regexp.Regexp
			if useRegex {
				re, err = regexp.Compile(pattern)
				if err != nil {
					return fmt.Errorf("invalid regex %q: %w", pattern, err)
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
			printScopePreamble(scope, asJSON)
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
		Usage:     "show recent hook_runs (precreate/postcreate/predelete/postdelete) for a worktree",
		ArgsUsage: "[worktree]",
		Description: `Examples:
  treeman logs hooks                # cwd-resolved worktree
  treeman logs hooks PROJ-1234
  treeman logs hooks --all          # every worktree
  treeman logs hooks --show 42      # render captured stdout+stderr for run id 42
  treeman logs hooks --json | jq .

The worktree argument is optional — when omitted, the worktree
containing the current working directory is used. Pass --all to
span every worktree (e.g. when running from outside any repo).

--show takes a hook_run id (from the ID column) and writes the
captured merged stdout+stderr to stdout verbatim — ANSI escapes
included, so the original terminal colors round-trip.`,
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "n", Value: 20, Usage: "max rows"},
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo root override"},
			&cli.BoolFlag{Name: "all", Aliases: []string{"A"}, Usage: "show hook runs from every worktree (skips cwd auto-resolve)"},
			&cli.IntFlag{Name: "show", Usage: "render captured stdout+stderr for the given hook_run id"},
			&cli.BoolFlag{
				Name:  "view",
				Usage: "with --show: interactive scroll/search viewer (/ jump, n/N repeat, q quit) instead of a verbatim dump",
			},
			&cli.BoolFlag{Name: "json"},
			&cli.BoolFlag{Name: "no-pager", Usage: "disable the pager even when stdout is a TTY"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			st, closer, err := openLogStore(ctx)
			if err != nil {
				return err
			}
			defer closer()

			if id := int64(c.Int("show")); id > 0 {
				return renderHookLog(ctx, st, id, c.Bool("json"), c.Bool("view"), c)
			}
			all := c.Bool("all")
			wtID, name, err := resolveHooksScope(ctx, st, c, all)
			if err != nil {
				return err
			}

			runs, err := st.QueryHookRuns(ctx, wtID, c.Int("n"))
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(runs)
			}
			if len(runs) == 0 {
				if all {
					ui.Info("no hook runs recorded")
				} else {
					ui.Info("no hook runs recorded for %s", name)
				}
				return nil
			}
			pager := newPagerIfEligible(c, false, false)
			if pager != nil {
				_ = pager.Start()
				defer pager.Close()
			}
			renderHookRunsTable(runs, all)
			return nil
		},
	}
}

// resolveHooksScope resolves the worktree id + display name to scope
// `logs hooks` to, honoring --repo/--all and cwd auto-detection. Returns
// (0, "", nil) for the --all span (no single worktree).
func resolveHooksScope(ctx context.Context, st *store.Store, c *cli.Command, all bool) (int64, string, error) {
	repoID := int64(0)
	var err error
	switch {
	case c.String("repo") != "":
		repoID, err = lookupExplicitRepoID(ctx, st, c.String("repo"))
	case !all:
		repoID, err = resolveShowRepoID(ctx, st, "")
	}
	if err != nil {
		return 0, "", err
	}

	if c.NArg() >= 1 {
		name := c.Args().First()
		wtID, lookupErr := st.LookupWorktreeID(ctx, repoID, name)
		if lookupErr != nil {
			return 0, "", lookupErr
		}
		if wtID == 0 {
			return 0, "", fmt.Errorf("no worktree matches %q (try `treeman worktree list`)", name)
		}
		return wtID, name, nil
	}
	if all {
		return 0, "", nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return 0, "", fmt.Errorf("resolve the current directory to find its worktree: %w", err)
	}
	row, err := lookupWorktreeContainingCwd(ctx, st, cwd)
	if err != nil {
		return 0, "", err
	}
	if row.ID == 0 {
		return 0, "", errors.New(
			"usage: treeman logs hooks [worktree] (cwd is not inside a registered worktree; pass --all to span every worktree)",
		)
	}
	name := row.Slug
	if name == "" {
		name = row.Branch
	}
	if !c.Bool("json") {
		fmt.Fprintf(os.Stderr, "# scope: worktree=%s (--all to widen)\n", name)
	}
	return row.ID, name, nil
}

// renderHookRunsTable prints the human-readable hook_runs table. The
// WORKTREE column only appears in --all mode.
func renderHookRunsTable(runs []store.HookRun, all bool) {
	cols := []string{"ID", "STARTED", "PHASE", "GROUP", "EXIT", "DURATION", "COMMAND"}
	if all {
		cols = []string{"ID", "STARTED", "WORKTREE", "PHASE", "GROUP", "EXIT", "DURATION", "COMMAND"}
	}
	tbl := ui.NewTable(cols...)
	for _, h := range runs {
		var exit string
		if h.ExitCode.Valid {
			code := strconv.FormatInt(h.ExitCode.Int64, 10)
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
		cmd := h.Command
		if cmd == "" {
			// Older rows (pre-0008) won't have command; fall
			// back to the captured tails so the column isn't
			// empty.
			cmd = h.StderrTail
			if cmd == "" {
				cmd = h.StdoutTail
			}
		}
		cmd = singleLine(cmd, 80)
		group := strconv.FormatInt(h.GroupIdx, 10)
		id := ui.Dim(strconv.FormatInt(h.ID, 10))
		if all {
			slug := h.WorktreeSlug
			if slug == "" {
				slug = fmt.Sprintf("#%d", h.WorktreeID)
			}
			tbl.Row(id, ui.Dim(formatTs(h.StartedAt)), ui.Magenta(slug), ui.Cyan(h.Phase), ui.Dim(group), exit, dur, cmd)
		} else {
			tbl.Row(id, ui.Dim(formatTs(h.StartedAt)), ui.Cyan(h.Phase), ui.Dim(group), exit, dur, cmd)
		}
	}
	// Same full-fidelity rule as the tail renderer: width-fitting that
	// truncates the COMMAND column only happens on a real terminal;
	// piped output keeps complete commands for grep + captures.
	if ui.StdoutIsTerminal() {
		tbl.SetWidth(ui.TermWidth())
	}
	tbl.Render(nil)
}

// renderHookLog writes the captured stdout+stderr for a hook_run id
// to stdout. Bytes are streamed verbatim so ANSI escape codes
// (colors, cursor moves, hyperlinks) round-trip from the original
// hook subprocess. JSON mode emits one envelope per chunk with the
// raw body inline (already base64 by encoding/json when the chunk
// contains non-UTF8 bytes).
func renderHookLog(ctx context.Context, st *store.Store, id int64, asJSON, view bool, c *cli.Command) error {
	chunks, err := st.QueryHookLog(ctx, id)
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		return fmt.Errorf("no captured log for hook_run id=%d", id)
	}
	if asJSON {
		return jsonStream(chunks)
	}
	// Interactive viewer (--view): the whole captured log in a
	// scroll/search viewport (#65). Piped terminals degrade to the
	// verbatim dump below.
	if view {
		var b strings.Builder
		for _, chunk := range chunks {
			b.Write(chunk.Body)
		}
		if verr := tui.ViewContent(fmt.Sprintf("hook_run %d", id), b.String()); verr == nil {
			return nil
		} else if !errors.Is(verr, tui.ErrNotTTY) {
			return verr
		}
		ui.Warn("--view needs a terminal; dumping verbatim")
	}
	// A long build log scrolls past unpaged — same pager policy as
	// `logs tail` (#65): $PAGER on a TTY, verbatim bytes otherwise
	// (--json handled above, --no-pager via the command flags).
	pager := newPagerIfEligible(c, false, asJSON)
	if pager != nil {
		_ = pager.Start()
		defer pager.Close()
	}
	for _, c := range chunks {
		_, _ = ui.Out.Write(c.Body)
	}
	return nil
}

// filterScope carries the EventFilter plus the resolved worktree
// metadata used to print a one-line preamble explaining why the
// filter is what it is.
type filterScope struct {
	Filter       store.EventFilter
	WorktreeName string // empty when not scoped to a worktree
	WorktreePath string // empty when not scoped to a worktree
	AutoResolved bool   // true when --worktree was not passed but cwd matched a row
}

func buildFilterWithScope(ctx context.Context, c *cli.Command) (filterScope, error) {
	levels, err := store.ParseLevels(c.StringSlice("level"))
	if err != nil {
		return filterScope{}, err
	}
	scope := filterScope{
		Filter: store.EventFilter{
			Levels:      levels,
			EventTypes:  c.StringSlice("event-type"),
			Phases:      c.StringSlice("phase"),
			PayloadLike: c.String("payload"),
			HydrateWT:   true,
		},
	}
	if s := c.String("since"); s != "" {
		t, err := parseSince(s)
		if err != nil {
			return scope, err
		}
		scope.Filter.SinceMs = t.UnixMilli()
	}

	st, closer, err := openLogStore(ctx)
	if err != nil {
		return scope, err
	}
	defer closer()

	all := c.Bool("all")
	repoOverride := c.String("repo")
	wantWT := c.String("worktree")

	// --all (with no narrower override) means "show everything" — skip
	// cwd-based scoping entirely so the caller doesn't get silently
	// filtered to the repo / worktree they happen to be standing in.
	if all && repoOverride == "" && wantWT == "" {
		return scope, nil
	}

	// Resolve repo ID from --repo or cwd. An explicit --repo is also a
	// signal to skip the cwd→worktree auto-resolve below: the caller
	// asked for "this repo's events", not "this repo's events filtered
	// to whichever worktree my shell happens to be in".
	if scope.Filter.RepoID, err = resolveShowRepoID(ctx, st, repoOverride); err != nil {
		return scope, err
	}

	// Explicit --worktree wins over cwd auto-resolve.
	if wantWT != "" {
		id, err := st.LookupWorktreeID(ctx, scope.Filter.RepoID, wantWT)
		if err != nil {
			return scope, err
		}
		if id == 0 {
			return scope, fmt.Errorf("no worktree matches %q (try `treeman worktree list`)", wantWT)
		}
		scope.Filter.WorktreeID = id
		scope.WorktreeName = wantWT
		return scope, nil
	}

	// --all OR explicit --repo both short-circuit the cwd-worktree
	// resolver: the user gave us a scope already.
	if all || repoOverride != "" {
		return scope, nil
	}

	// Auto-resolve: cwd → worktree row.
	cwd, err := os.Getwd()
	if err != nil {
		return scope, fmt.Errorf("resolve the current directory to scope the logs to its worktree: %w", err)
	}
	row, err := lookupWorktreeContainingCwd(ctx, st, cwd)
	if err != nil {
		return scope, err
	}
	if row.ID == 0 {
		return scope, nil
	}
	scope.Filter.WorktreeID = row.ID
	if row.RepoID > 0 {
		scope.Filter.RepoID = row.RepoID
	}
	scope.WorktreeName = row.Slug
	if scope.WorktreeName == "" {
		scope.WorktreeName = row.Branch
	}
	scope.WorktreePath = row.Path
	scope.AutoResolved = true
	return scope, nil
}

// lookupWorktreeContainingCwd finds the worktree row whose `path`
// is a prefix of (or equal to) cwd. The longest prefix wins so
// nested worktrees resolve to the most specific row. Returns the
// zero value when no row matches — callers treat that as
// "logs scope = all worktrees".
func lookupWorktreeContainingCwd(ctx context.Context, st *store.Store, cwd string) (store.WorktreeRow, error) {
	row, err := st.LookupActiveWorktreeByPath(ctx, cwd)
	if err != nil {
		return store.WorktreeRow{}, fmt.Errorf("look up the worktree at %s: %w", cwd, err)
	}
	if row.ID != 0 {
		return row, nil
	}
	rows, err := st.DB.QueryContext(ctx, `
		SELECT id, repo_id, path, slug, COALESCE(branch, ''), COALESCE(admin_dir, ''), 0
		FROM worktrees
		WHERE deleted_at IS NULL AND ? LIKE path || '/%' COLLATE NOCASE
		ORDER BY length(path) DESC
		LIMIT 1`, cwd)
	if err != nil {
		return store.WorktreeRow{}, fmt.Errorf("find the worktree containing %s: %w", cwd, err)
	}
	defer func() { _ = rows.Close() }()
	var w store.WorktreeRow
	if rows.Next() {
		var deleted int
		if err := rows.Scan(&w.ID, &w.RepoID, &w.Path, &w.Slug, &w.Branch, &w.AdminDir, &deleted); err != nil {
			return store.WorktreeRow{}, fmt.Errorf("scan the worktree containing %s: %w", cwd, err)
		}
	}
	if err := rows.Err(); err != nil {
		return store.WorktreeRow{}, fmt.Errorf("find the worktree containing %s: %w", cwd, err)
	}
	return w, nil
}

// printScopePreamble writes a one-line scope hint to stderr when the
// cwd auto-resolver picked a worktree. Skipped for --json + --all to
// keep machine output and explicit "show everything" runs quiet.
func printScopePreamble(s filterScope, asJSON bool) {
	if asJSON || !s.AutoResolved || s.WorktreeName == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "# scope: worktree=%s (--all to widen)\n", s.WorktreeName)
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
	st, err := openDefaultStore(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	return st, func() { _ = st.Close() }, nil
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
	printEventStyled(eventStyle{asJSON: asJSON}, e)
}

// eventStyle carries the human-mode presentation knobs for printEvent.
// JSON output ignores all of them (byte-identical to the unstyled
// printer).
type eventStyle struct {
	asJSON  bool
	verbose bool // append sorted dim key=value pairs from the payload
	fullTS  bool // always the full date+time, even for today's events
}

// payloadNoiseKeys are structured bookkeeping that adds nothing when
// inlined into a human line (the run is already the line's context).
var payloadNoiseKeys = map[string]bool{"run_id": true}

// printEventStyled renders one row: colored, padded human line or a
// flat JSON object. With verbose on, the payload's structured facts
// are appended as sorted dim key=value pairs (#81); by default today's
// events print time-of-day only (--full-ts restores the full stamp).
func printEventStyled(style eventStyle, e store.Event) {
	if style.asJSON {
		_ = jsonStream([]store.Event{e})
		return
	}
	level := ui.Level(e.Level)
	et := ui.EventType(e.EventType)
	ts := ui.Dim(formatTsStyled(style.fullTS, e.Ts))
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
	payload := ""
	if style.verbose {
		payload = " " + ui.Dim(sortedPayloadPairs(e.PayloadJSON))
	}
	// Fit the message to the terminal: the prefix (ts, level, 24-rune
	// event-type pad, worktree, phase) and the duration suffix are
	// reserved first, and an overlong message gets an ellipsis instead
	// of wrapping the rest of the line. JSON output (handled above)
	// stays full-length, and so does PIPED output — the terminal width
	// is unknown there, and a mid-message ellipsis breaks grep and
	// captured CI logs (the #69 regression the logs e2e guards).
	msg := e.Message
	if !style.asJSON && ui.StdoutIsTerminal() {
		budget := max(ui.TermWidth()-ui.Width(ts)-1-ui.Width(level)-1-24-ui.Width(wt)-ui.Width(phase)-1-ui.Width(dur)-ui.Width(payload), 10)
		msg = ui.Truncate(msg, budget)
	}
	_, _ = fmt.Fprintf(ui.Out, "%s %s %s%s%s %s%s%s\n", ts, padRight(level, 5), padRight(et, 24), wt, phase, msg, dur, payload)
}

// sortedPayloadPairs renders the event payload as `k=v` pairs, keys
// sorted for stable output, noise keys dropped. A malformed payload
// degrades to empty — the message column already carries the story.
func sortedPayloadPairs(payloadJSON string) string {
	if payloadJSON == "" || payloadJSON == "{}" {
		return ""
	}
	var kv map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &kv); err != nil {
		return ""
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		if !payloadNoiseKeys[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, kv[k]))
	}
	return strings.Join(parts, " ")
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
	case []store.HookLogChunk:
		for _, c := range x {
			if err := enc.Encode(c); err != nil {
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
// block our reads. On a TTY in human mode it opens with a dim
// "following" banner on stderr (#101) — stdout stays pipe-clean — and
// when a batch arrives after an idle gap (previous event >2 minutes
// earlier), a dim timestamp separator narrates the scrollback.
func followLoop(ctx context.Context, st *store.Store, baseFilter store.EventFilter, lastID int64, style eventStyle) error {
	baseFilter.OldestFirst = true
	baseFilter.Limit = 0
	baseFilter.AfterID = lastID
	if !style.asJSON {
		_, _ = fmt.Fprintln(ui.Err, ui.Dim("following — ctrl+c to stop"))
	}
	// lastMs is the ts of the last printed event (0 until the first
	// batch); startedAt lets the FIRST batch also get a separator when
	// the backlog it resumes from is stale — the "is this new or the
	// startup backlog?" ambiguity #101 is about.
	var lastMs int64
	startedAt := time.Now().UnixMilli()
	gap := 2 * time.Minute.Milliseconds()
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
				if !style.asJSON && (e.Ts-lastMs > gap || (lastMs == 0 && startedAt-e.Ts > gap)) {
					_, _ = fmt.Fprintf(ui.Err, "%s\n", ui.Dim("──── "+time.UnixMilli(e.Ts).Format("15:04:05")+" ────"))
				}
				lastMs = e.Ts
				printEventStyled(style, e)
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
