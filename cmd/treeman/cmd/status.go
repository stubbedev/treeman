package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/template"
)

// The four buckets every active worktree falls into. `up`/`down` are
// transient (finalize / teardown in flight); `stable` and `failed`
// are the resting states.
const (
	bucketStable = "stable"
	bucketUp     = "up"
	bucketDown   = "down"
	bucketFailed = "failed"
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
  icon    one-line counter (default), plain text
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
				Value:   "icon",
				Usage:   "icon | hover | waybar | json | <name from status.formats>",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			gcfg, err := config.LoadGlobal()
			if err != nil {
				return err
			}
			data, err := collectStatus(ctx)
			if err != nil {
				return err
			}
			out, err := renderStatus(c.String("format"), data, gcfg.Status)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
}

// collectStatus reads every active worktree (across all repos) and
// derives its bucket. Reads the store directly — no daemon round-trip
// — so the widget keeps working while the daemon is restarting.
func collectStatus(ctx context.Context) (statusData, error) {
	dbPath, err := store.DefaultDBPath()
	if err != nil {
		return statusData{}, err
	}
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return statusData{}, err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.DB.QueryContext(ctx, `
		SELECT w.id, COALESCE(w.slug,''), COALESCE(w.branch,'-'), w.path, w.is_main, r.path
		FROM worktrees w JOIN repos r ON r.id = w.repo_id
		WHERE w.deleted_at IS NULL
		ORDER BY r.path, w.is_main DESC, w.branch`)
	if err != nil {
		return statusData{}, err
	}
	defer func() { _ = rows.Close() }()

	var (
		data      statusData
		repoOrder []string
		byRepo    = map[string]*statusRepo{}
	)
	for rows.Next() {
		var (
			id           int64
			slug, branch string
			wpath, rpath string
			isMain       bool
		)
		if err := rows.Scan(&id, &slug, &branch, &wpath, &isMain, &rpath); err != nil {
			return statusData{}, err
		}
		state, bucket := deriveStatusBucket(ctx, st, id)
		data.Total++
		switch bucket {
		case bucketStable:
			data.Stable++
		case bucketUp:
			data.Up++
		case bucketDown:
			data.Down++
		case bucketFailed:
			data.Failed++
		}
		rs, ok := byRepo[rpath]
		if !ok {
			rs = &statusRepo{Repo: filepath.Base(rpath)}
			byRepo[rpath] = rs
			repoOrder = append(repoOrder, rpath)
		}
		rs.Total++
		rs.Worktrees = append(rs.Worktrees, statusWt{
			Branch: branch,
			Slug:   slug,
			State:  state,
			Bucket: bucket,
			IsMain: isMain,
			Path:   wpath,
		})
	}
	if err := rows.Err(); err != nil {
		return statusData{}, err
	}
	for _, rp := range repoOrder {
		data.Repos = append(data.Repos, *byRepo[rp])
	}
	data.Class = worstClass(data)
	return data, nil
}

// deriveStatusBucket maps a worktree's most recent lifecycle event to
// a (state, bucket) pair. Mirrors finalizeState but also folds in the
// teardown events so an in-flight teardown surfaces as `down`. A
// worktree with no events yet is treated as ready/stable.
func deriveStatusBucket(ctx context.Context, st *store.Store, wtID int64) (state, bucket string) {
	rows, _ := st.QueryEvents(ctx, store.EventFilter{
		WorktreeID: wtID,
		EventTypes: []string{
			store.EvtWorktreeCreateStart, store.EvtWorktreeCreateEnd, store.EvtWorktreeCreateError,
			store.EvtWorktreeDeleteStart, store.EvtWorktreeDeleteEnd,
			store.EvtWorktreeReapStart, store.EvtWorktreeReapEnd,
			// A standalone prepare_run (manual or via repair) emits
			// prepare:* but no worktree:create:end, so a successful
			// recovery after a prior create error must be read off the
			// prepare terminal events too — otherwise the stale error
			// pins the worktree to "failed" forever.
			store.EvtPrepareEnd, store.EvtPrepareError,
		},
		Limit: 1,
	})
	if len(rows) == 0 {
		return "ready", bucketStable
	}
	switch last := rows[0]; last.EventType {
	case store.EvtWorktreeCreateStart:
		return "preparing", bucketUp
	case store.EvtWorktreeCreateError, store.EvtPrepareError:
		if last.Level == store.LevelError {
			return "error", bucketFailed
		}
		return "ready", bucketStable
	case store.EvtWorktreeDeleteStart, store.EvtWorktreeReapStart:
		return "teardown", bucketDown
	default:
		// worktree:create:end / worktree:delete:end / worktree:reap:end.
		return "ready", bucketStable
	}
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
	return "", fmt.Errorf("unknown --format %q (icon|hover|waybar|json or a name from status.formats)", format)
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
		case bucketStable:
			stable++
		case bucketUp:
			up++
		case bucketDown:
			down++
		case bucketFailed:
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
	if wt.Bucket != bucketStable {
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
	case bucketUp:
		return cfg.Icons.Up
	case bucketDown:
		return cfg.Icons.Down
	case bucketFailed:
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
