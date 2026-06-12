package cmd

import (
	"context"
	"strconv"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/ui"
)

// ReposCmd — `treeman repos` lists every repo enrolled in the SQLite
// registry: active worktree count, main-mode enrollment, cached
// snapshots, and last recorded activity. The same surface is wired
// under `treeman registry list` so it's discoverable from both ends.
func ReposCmd() *cli.Command {
	c := reposListCommand()
	c.Name = "repos"
	c.Usage = "list repos enrolled in treeman"
	return c
}

// RegistryListCmd is the `treeman registry list` spelling of the same
// command. Kept separate from ReposCmd so each parent gets its own
// *cli.Command instance (urfave/cli mutates commands during setup).
func RegistryListCmd() *cli.Command {
	c := reposListCommand()
	c.Name = "list"
	c.Aliases = []string{"ls"}
	c.Usage = "list repos enrolled in the registry"
	return c
}

// repoRow is one row of `treeman repos` output.
type repoRow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Worktrees    int    `json:"worktrees"`
	MainEnrolled bool   `json:"main_enrolled"`
	Snapshots    int    `json:"snapshots"`
	RegisteredTs int64  `json:"registered_ts"`
	LastEventTs  int64  `json:"last_event_ts,omitempty"`
}

func reposListCommand() *cli.Command {
	return &cli.Command{
		Description: `Reads the SQLite registry directly (no daemon round-trip), so it
works while the daemon is down. A repo is enrolled the first time
treeman touches it (wt create, prepare, or daemon watch); drop one
with ` + "`treeman registry remove`" + `.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			rows, err := collectRepoRows(ctx)
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(rows)
			}
			if len(rows) == 0 {
				ui.Info("no repos enrolled")
				ui.Hint("%s", "enroll one by running `treeman init` then `treeman prepare` inside a repo")
				return nil
			}
			renderRepoTable(rows)
			return nil
		},
	}
}

// collectRepoRows reads every registered repo plus its aggregate
// counts in one query. Reads the store directly — same rationale as
// collectStatus: the listing must work while the daemon restarts.
func collectRepoRows(ctx context.Context) ([]repoRow, error) {
	st, closeStore, err := openLogStore(ctx)
	if err != nil {
		return nil, err
	}
	defer closeStore()

	rows, err := st.DB.QueryContext(ctx, `
		SELECT r.id, r.name, r.path, r.registered_at,
			(SELECT COUNT(*) FROM worktrees w WHERE w.repo_id = r.id AND w.deleted_at IS NULL),
			(SELECT COUNT(*) FROM worktrees w WHERE w.repo_id = r.id AND w.deleted_at IS NULL AND w.is_main = 1),
			(SELECT COUNT(*) FROM snapshots s WHERE s.repo_id = r.id),
			COALESCE((SELECT MAX(e.ts) FROM events e WHERE e.repo_id = r.id), 0)
		FROM repos r ORDER BY r.path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []repoRow
	for rows.Next() {
		var (
			r     repoRow
			mains int
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.Path, &r.RegisteredTs, &r.Worktrees, &mains, &r.Snapshots, &r.LastEventTs); err != nil {
			return nil, err
		}
		r.MainEnrolled = mains > 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// renderRepoTable prints the human-readable `treeman repos` table.
// Column style mirrors renderWtTable: cyan identity column, dim
// timestamps and paths, ★ marker for main-mode enrollment.
func renderRepoTable(all []repoRow) {
	anyMain := false
	for _, r := range all {
		if r.MainEnrolled {
			anyMain = true
			break
		}
	}
	headers := []string{"REPO"}
	if anyMain {
		headers = append(headers, "MAIN")
	}
	headers = append(headers, "WORKTREES", "SNAPSHOTS", "LAST", "PATH")
	tbl := ui.NewTable(headers...)
	for _, r := range all {
		cells := []string{ui.Cyan(r.Name)}
		if anyMain {
			if r.MainEnrolled {
				cells = append(cells, ui.Cyan("★"))
			} else {
				cells = append(cells, "")
			}
		}
		// lastLabel's second arg is epoch millis — matches events.ts.
		cells = append(cells,
			strconv.Itoa(r.Worktrees),
			strconv.Itoa(r.Snapshots),
			ui.Dim(lastLabel(0, r.LastEventTs)),
			r.Path,
		)
		tbl.Row(cells...)
	}
	tbl.Render(nil)
}
