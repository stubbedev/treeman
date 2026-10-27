package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
	"github.com/stubbedev/treeman/internal/ui"
)

// defaultIconFormat is the built-in `icon` line when the user hasn't
// declared a `status.formats.icon` override. Rendered through the same
// `{key}` engine as a custom format so the two paths can't diverge.
// Leads each segment with the bucket glyph, then the label and count.
const defaultIconFormat = "{icon_stable} {label_stable}: {stable}{sep}{icon_up} {label_up}: {up}{sep}{icon_down} {label_down}: {down}{sep}{icon_failed} {label_failed}: {failed}"

type statusWt struct {
	Branch string `json:"branch"`
	Slug   string `json:"slug"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
	IsMain bool   `json:"is_main"`
	Path   string `json:"path"`
	// AgeTs is the unix-ms of the worktree's latest lifecycle event —
	// the "how long has it been in this state" input for the table
	// format's AGE column (#57). 0 when no event exists yet.
	AgeTs int64 `json:"age_ts"`
}

type statusRepo struct {
	Repo      string     `json:"repo"`
	Total     int        `json:"total"`
	Worktrees []statusWt `json:"worktrees"`
}

type statusData struct {
	Total  int          `json:"total"`
	Stable int          `json:"stable"`
	Up     int          `json:"up"`
	Down   int          `json:"down"`
	Failed int          `json:"failed"`
	Class  string       `json:"class"`
	Repos  []statusRepo `json:"repos"`
}

// StatusCmd — `treeman status [--format ...]`. Aggregates worktree
// health across every registered repo and renders it for a status-bar
// widget. The default `icon` and `hover` formats print plain text
// (like `cal`); `waybar` emits the `{text,tooltip,class}` JSON a
// waybar custom module consumes; `json` emits the raw shape.
func StatusCmd() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "summarize worktree health across all repos (bar/waybar widget)",
		Description: `Aggregates every active worktree across all registered repos into
four buckets — stable (ready), up (preparing), down (tearing down),
failed (last finalize errored) — and renders them.

Formats (--format):
  table   aligned per-worktree table, colored states + relative age
          (default when stdout is a TTY)
  icon    one-line counter, plain text (default when piped)
  hover   per-repo grouped detail, plain text (the "cal-style" block)
  waybar  {"text","tooltip","class"} JSON for a waybar custom module
  json    the raw aggregated shape (counts + per-repo worktrees)
  <name>  a custom single-line {key} format from status.formats

Icons, labels, separators, the hover header/row templates, and custom
formats are all configured under the global config's status: block.`,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "format",
				Aliases: []string{"f"},
				Value:   "",
				Usage:   "table | icon | hover | waybar | json | <name from status.formats> (default: table on a TTY, else icon)",
			},
			&cli.StringFlag{
				Name:  "repo",
				Value: "",
				Usage: "limit the summary to the repo rooted at <path> (default: all registered repos)",
			},
			&cli.BoolFlag{
				Name:  "watch",
				Usage: "re-render in place on an interval until Ctrl-C (terminal formats only; see --watch-interval)",
			},
			&cli.DurationFlag{
				Name:  "watch-interval",
				Value: time.Second,
				Usage: "refresh interval for --watch (e.g. 2s, 500ms)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			gcfg, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			format := c.String("format")
			if format == "" {
				// Humans get the table, scripts keep the icon line — a
				// piped `treeman status` must not change shape.
				format = "icon"
				if ui.IsTTY() {
					format = "table"
				}
			}
			if c.Bool("watch") {
				// Machine formats are one-shot shapes (a widget or a jq
				// pipeline re-invokes the command itself); --watch is the
				// terminal experience.
				switch format {
				case "json", "waybar":
					return fmt.Errorf("--watch is for terminal formats (table/icon/hover); --format %s is a one-shot machine shape", format)
				}
				return watchStatus(ctx, c.String("repo"), format, gcfg.Status, c.Duration("watch-interval"))
			}
			data, err := collectStatus(ctx, c.String("repo"))
			if err != nil {
				return err
			}
			out, err := renderStatus(format, data, gcfg.Status)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
}

// watchStatus re-collects + re-renders on a ticker, redrawing in place
// via ANSI cursor-home (NO_COLOR / non-TTY degrades to plain repeated
// prints, still correct — just scrollier). Ctrl-C ends the loop; a
// store error prints once and keeps retrying so a daemon restart
// doesn't kill the watch.
func watchStatus(ctx context.Context, repoFilter, format string, cfg config.StatusConfig, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		data, err := collectStatus(ctx, repoFilter)
		if err == nil {
			out, rerr := renderStatus(format, data, cfg)
			if rerr == nil {
				fmt.Print("\x1b[H\x1b[2J" + out + "\n")
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// collectStatus reads every active worktree (across all repos) and
// derives its bucket. Reads the store directly — no daemon round-trip
// — so the widget keeps working while the daemon is restarting.
func collectStatus(ctx context.Context, repoFilter string) (statusData, error) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return statusData{}, err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return statusData{}, err
	}
	defer func() { _ = st.Close() }()

	query := `
		SELECT w.id, COALESCE(w.slug,''), COALESCE(w.branch,'-'), w.path, w.is_main, r.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL`
	args := []any{}
	if repoFilter != "" {
		query += ` AND r.path = ? COLLATE NOCASE`
		args = append(args, repoFilter)
	}
	query += `
		ORDER BY r.path, w.is_main DESC, w.branch`
	rows, err := st.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return statusData{}, err
	}
	defer func() { _ = rows.Close() }()

	var (
		data      statusData
		repoOrder []string
		byRepo    = map[string]*statusRepo{}
	)
	ids := make([]int64, 0, 16)
	var scanned []statusWtWithID
	for rows.Next() {
		var r statusWtWithID
		if err := rows.Scan(&r.id, &r.wt.Slug, &r.wt.Branch, &r.wt.Path, &r.wt.IsMain, &r.repoPath); err != nil {
			return statusData{}, err
		}
		ids = append(ids, r.id)
		scanned = append(scanned, r)
	}
	if err := rows.Err(); err != nil {
		return statusData{}, err
	}
	// One query for every row's latest lifecycle event — the bar widget
	// used to pay one QueryEvents per worktree per tick.
	latest, err := st.LatestWorktreeStates(ctx, ids)
	if err != nil {
		return statusData{}, err
	}
	for _, r := range scanned {
		state, bucket := store.DeriveWorktreeState(latest[r.id])
		data.Total++
		switch bucket {
		case store.BucketStable:
			data.Stable++
		case store.BucketUp:
			data.Up++
		case store.BucketDown:
			data.Down++
		case store.BucketFailed:
			data.Failed++
		}
		rs, ok := byRepo[r.repoPath]
		if !ok {
			rs = &statusRepo{Repo: filepath.Base(r.repoPath)}
			byRepo[r.repoPath] = rs
			repoOrder = append(repoOrder, r.repoPath)
		}
		rs.Total++
		r.wt.State = state
		r.wt.Bucket = bucket
		r.wt.AgeTs = latest[r.id].Ts
		rs.Worktrees = append(rs.Worktrees, r.wt)
	}
	for _, rp := range repoOrder {
		data.Repos = append(data.Repos, *byRepo[rp])
	}
	data.Class = worstClass(data)
	return data, nil
}

// statusWtWithID pairs a status row with the ids/paths needed for the
// batched-event second pass.
type statusWtWithID struct {
	wt       statusWt
	id       int64
	repoPath string
}

// worstClass is the waybar `class` (and `{class}` token): the most
// severe non-resting condition present. failed > active (up|down) >
// "" (all stable). Matches the existing waybar CSS selectors.
func worstClass(d statusData) string {
	switch {
	case d.Failed > 0:
		return "failed"
	case d.Up > 0 || d.Down > 0:
		return "active"
	default:
		return ""
	}
}

// renderStatus dispatches on the format name. The structured/multi-line
// built-ins (hover, waybar, json) are reserved; everything else is a
// single-line `{key}` template — either a `status.formats` entry
// (which may override `icon`) or the default icon line.
func renderStatus(format string, d statusData, cfg config.StatusConfig) (string, error) {
	switch format {
	case "table":
		return renderStatusTable(d, cfg), nil
	case "hover":
		return renderHover(d, cfg)
	case "waybar":
		return renderWaybar(d, cfg)
	case "json":
		b, err := json.Marshal(d)
		return string(b), err
	}
	if tmpl, ok := cfg.Formats[format]; ok {
		return template.RenderMap(tmpl, statusTokens(d, cfg))
	}
	if format == "" || format == "icon" {
		return template.RenderMap(defaultIconFormat, statusTokens(d, cfg))
	}
	return "", fmt.Errorf("unknown --format %q (table|icon|hover|waybar|json or a name from status.formats)", format)
}

// renderStatusTable renders the human-facing aligned table (#57):
// bucket icon, ui.Status-colored state, branch, slug, relative age,
// repo — one row per worktree. Color tokens are ANSI-safe through
// ui.Table's width math, so NO_COLOR stays aligned too.
func renderStatusTable(d statusData, cfg config.StatusConfig) string {
	icons := map[string]string{
		store.BucketStable: cfg.Icons.Stable,
		store.BucketUp:     cfg.Icons.Up,
		store.BucketDown:   cfg.Icons.Down,
		store.BucketFailed: cfg.Icons.Failed,
	}
	tbl := ui.NewTable("", "STATE", "BRANCH", "SLUG", "AGE", "REPO")
	for _, repo := range d.Repos {
		for _, wt := range repo.Worktrees {
			age := "—"
			if wt.AgeTs > 0 {
				age = lastLabel(wt.AgeTs/1000, 0)
			}
			tbl.Row(
				icons[wt.Bucket],
				ui.Status(wt.State),
				wt.Branch,
				wt.Slug,
				age,
				repo.Repo,
			)
		}
	}
	var b strings.Builder
	tbl.SetWidth(ui.TermWidth())
	tbl.Render(&b)
	return strings.TrimRight(b.String(), "\n")
}

// renderWaybar assembles the `{text,tooltip,class}` object a waybar
// custom module reads. text reuses the icon line (honoring any
// `status.formats.icon` override); tooltip is the hover block.
func renderWaybar(d statusData, cfg config.StatusConfig) (string, error) {
	text, err := renderStatus("icon", d, cfg)
	if err != nil {
		return "", err
	}
	tooltip, err := renderHover(d, cfg)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Text    string `json:"text"`
		Tooltip string `json:"tooltip"`
		Class   string `json:"class"`
	}{Text: text, Tooltip: tooltip, Class: d.Class})
	return string(b), err
}

// renderHover renders the multi-line, per-repo detail block. The repo
// header and each worktree row come from the configured `{key}`
// templates; repos are separated by a blank line.
func renderHover(d statusData, cfg config.StatusConfig) (string, error) {
	blocks := make([]string, 0, len(d.Repos))
	for _, repo := range d.Repos {
		hdr, err := template.RenderMap(cfg.Header, repoTokens(repo))
		if err != nil {
			return "", err
		}
		lines := make([]string, 0, len(repo.Worktrees)+1)
		lines = append(lines, hdr)
		for _, wt := range repo.Worktrees {
			row, err := template.RenderMap(cfg.Row, wtTokens(wt, cfg))
			if err != nil {
				return "", err
			}
			lines = append(lines, row)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n"), nil
}

// statusTokens is the `{key}` namespace for single-line formats (the
// icon line and any custom `status.formats` entry).
func statusTokens(d statusData, cfg config.StatusConfig) map[string]string {
	return map[string]string{
		"total":        strconv.Itoa(d.Total),
		"stable":       strconv.Itoa(d.Stable),
		"up":           strconv.Itoa(d.Up),
		"down":         strconv.Itoa(d.Down),
		"failed":       strconv.Itoa(d.Failed),
		"icon_stable":  cfg.Icons.Stable,
		"icon_up":      cfg.Icons.Up,
		"icon_down":    cfg.Icons.Down,
		"icon_failed":  cfg.Icons.Failed,
		"icon":         worstIcon(d, cfg),
		"label_stable": cfg.Labels.Stable,
		"label_up":     cfg.Labels.Up,
		"label_down":   cfg.Labels.Down,
		"label_failed": cfg.Labels.Failed,
		"class":        d.Class,
		"sep":          cfg.Separator,
	}
}

// repoTokens is the `{key}` namespace for a hover repo header.
func repoTokens(r statusRepo) map[string]string {
	var stable, up, down, failed int
	for _, wt := range r.Worktrees {
		switch wt.Bucket {
		case store.BucketStable:
			stable++
		case store.BucketUp:
			up++
		case store.BucketDown:
			down++
		case store.BucketFailed:
			failed++
		}
	}
	return map[string]string{
		"repo":   r.Repo,
		"total":  strconv.Itoa(r.Total),
		"stable": strconv.Itoa(stable),
		"up":     strconv.Itoa(up),
		"down":   strconv.Itoa(down),
		"failed": strconv.Itoa(failed),
	}
}

// wtTokens is the `{key}` namespace for a hover worktree row.
// {main} resolves to the configured marker only on the main worktree;
// {state_suffix} is a parenthesised state for any non-stable row.
func wtTokens(wt statusWt, cfg config.StatusConfig) map[string]string {
	main := ""
	if wt.IsMain {
		main = cfg.MainMarker
	}
	suffix := ""
	if wt.Bucket != store.BucketStable {
		suffix = " (" + wt.State + ")"
	}
	return map[string]string{
		"branch":       wt.Branch,
		"slug":         wt.Slug,
		"state":        wt.State,
		"bucket":       wt.Bucket,
		"main":         main,
		"state_suffix": suffix,
		"path":         wt.Path,
		"icon":         bucketIcon(wt.Bucket, cfg),
	}
}

// bucketIcon returns the configured glyph for a single bucket.
func bucketIcon(bucket string, cfg config.StatusConfig) string {
	switch bucket {
	case store.BucketUp:
		return cfg.Icons.Up
	case store.BucketDown:
		return cfg.Icons.Down
	case store.BucketFailed:
		return cfg.Icons.Failed
	default:
		return cfg.Icons.Stable
	}
}

// worstIcon returns the glyph for the most severe bucket present —
// the `{icon}` token for single-line formats.
func worstIcon(d statusData, cfg config.StatusConfig) string {
	switch {
	case d.Failed > 0:
		return cfg.Icons.Failed
	case d.Down > 0:
		return cfg.Icons.Down
	case d.Up > 0:
		return cfg.Icons.Up
	default:
		return cfg.Icons.Stable
	}
}
