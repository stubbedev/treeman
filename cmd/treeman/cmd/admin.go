package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wtreg"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// RegistryCmd — `treeman registry {repair}` exposes the SQLite-side
// reconciliation primitives that doctor's `registry` check reports.
// (Per-worktree register/unregister already live under `treeman wt`.)
func RegistryCmd() *cli.Command {
	return &cli.Command{
		Name:  "registry",
		Usage: "SQLite worktree-registry maintenance",
		Commands: []*cli.Command{
			{
				Name:  "repair",
				Usage: "reconcile the SQLite registry with `git worktree list` (register drift / mark missing)",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo(c.String("repo"))
					if err != nil {
						return err
					}
					st, err := openDefaultStore(ctx)
					if err != nil {
						return err
					}
					defer st.Close()
					res, err := wtreg.Repair(ctx, st, repoRoot, detectBranchOfWorktree)
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return jsonStream(res)
					}
					for _, p := range res.Registered {
						PrintOK("registered %s", p)
					}
					for _, p := range res.Unregistered {
						PrintOK("unregistered %s", p)
					}
					for _, e := range res.Errors {
						PrintWarn("%s", e)
					}
					if len(res.Registered)+len(res.Unregistered) == 0 && len(res.Errors) == 0 {
						PrintInfo("registry already in sync with git")
					}
					return nil
				},
			},
		},
	}
}

// SnapshotsCmd — `treeman snapshots {list,purge}` exposes the cache
// the daemon's GC normally manages so an operator can inspect or
// force-purge it on demand.
func SnapshotsCmd() *cli.Command {
	return &cli.Command{
		Name:    "snapshots",
		Aliases: []string{"snap"},
		Usage:   "snapshot cache visibility + purge",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list cached snapshots (template DBs) for this repo",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo(c.String("repo"))
					if err != nil {
						return err
					}
					st, err := openDefaultStore(ctx)
					if err != nil {
						return err
					}
					defer st.Close()
					repoID, err := lookupRepoID(ctx, st, repoRoot)
					if err != nil {
						if c.Bool("json") {
							return jsonStream(map[string]any{"repo": repoRoot, "snapshots": []any{}})
						}
						PrintInfo("no snapshots recorded for %s", repoRoot)
						return nil
					}
					cands, err := st.ListSnapshotsForRepo(ctx, repoID)
					if err != nil {
						return err
					}
					if c.Bool("json") {
						return jsonStream(map[string]any{"repo": repoRoot, "snapshots": cands})
					}
					if len(cands) == 0 {
						PrintInfo("no snapshots cached for %s", repoRoot)
						return nil
					}
					t := ui.NewTable("ENGINE", "TEMPLATE", "SOURCE_DB", "FINGERPRINT")
					for _, sc := range cands {
						t.Row(ui.Cyan(sc.Engine), sc.TemplateName, sc.SourceDB, ui.Dim(sc.Fingerprint))
					}
					t.Render(nil)
					return nil
				},
			},
			{
				Name:  "purge",
				Usage: "drop every cached snapshot for this repo and force the next prepare to rebuild",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
					&cli.BoolFlag{Name: "json"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo(c.String("repo"))
					if err != nil {
						return err
					}
					cfg, err := resolve.LoadResolved(repoRoot)
					if err != nil {
						return fmt.Errorf("load config: %w", err)
					}
					st, err := openDefaultStore(ctx)
					if err != nil {
						return err
					}
					defer st.Close()
					repoID, err := st.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
					if err != nil {
						return fmt.Errorf("ensure repo: %w", err)
					}
					dropped, errs := snapshot.PurgeRepo(ctx, &cfg, st, repoID)
					if c.Bool("json") {
						msgs := make([]string, 0, len(errs))
						for _, e := range errs {
							msgs = append(msgs, e.Error())
						}
						return jsonStream(map[string]any{"repo": repoRoot, "dropped": dropped, "errors": msgs})
					}
					PrintOK("purged %d snapshot(s) for %s", dropped, repoRoot)
					for _, e := range errs {
						PrintWarn("%s", e)
					}
					return nil
				},
			},
		},
	}
}

// logsPurge is registered as a subcommand of `treeman logs`. Lives
// here so the read-only logs file stays focused on query paths.
func logsPurge() *cli.Command {
	return &cli.Command{
		Name:  "purge",
		Usage: "delete event-log rows by filter (at least one filter required)",
		Description: `At least one filter must be supplied so an unfiltered call can
never wipe the whole table.

  treeman logs purge --older-than 30d
  treeman logs purge --worktree PROJ-1234
  treeman logs purge --level debug --older-than 7d`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.StringFlag{Name: "worktree", Aliases: []string{"w"}},
			&cli.StringFlag{Name: "older-than", Usage: "duration (24h, 7d) or RFC3339 cutoff"},
			&cli.StringSliceFlag{Name: "level", Aliases: []string{"l"}},
			&cli.StringSliceFlag{Name: "event-type", Aliases: []string{"t"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.String("repo") == "" && c.String("worktree") == "" && c.String("older-than") == "" &&
				len(c.StringSlice("level")) == 0 && len(c.StringSlice("event-type")) == 0 {
				return fmt.Errorf("at least one filter (--repo, --worktree, --older-than, --level, --event-type) is required")
			}
			f := store.EventFilter{
				Levels:     validateLevels(c.StringSlice("level")),
				EventTypes: c.StringSlice("event-type"),
			}
			if v := c.String("older-than"); v != "" {
				t, err := parseOlderThan(v)
				if err != nil {
					return err
				}
				f.UntilMs = t.UnixMilli()
			}
			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()
			if c.String("repo") != "" || c.String("worktree") != "" {
				repoRoot, err := resolveRepo(c.String("repo"))
				if err == nil && repoRoot != "" {
					if rid, err := lookupRepoID(ctx, st, repoRoot); err == nil {
						f.RepoID = rid
					}
				}
				if wname := c.String("worktree"); wname != "" {
					wid, _ := st.LookupWorktreeID(ctx, f.RepoID, wname)
					if wid == 0 {
						return fmt.Errorf("no worktree matches %q", wname)
					}
					f.WorktreeID = wid
				}
			}
			n, err := st.PurgeEvents(ctx, f)
			if err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{"rows_removed": n})
			}
			PrintOK("removed %d event row(s)", n)
			return nil
		},
	}
}

// configSet returns the `treeman config set` subcommand. Wired into
// ConfigCmd by misc.go.
func configSet() *cli.Command {
	return &cli.Command{
		Name:      "set",
		Usage:     "patch a single field of .treeman.yaml by dotted path (preserves comments + key order)",
		ArgsUsage: "<path> <value>",
		Description: `<value> is parsed as JSON when possible (so 30, true, "x", ["a","b"] all
work) and falls back to a literal string otherwise.

Examples:
  treeman config set daemon.gc_interval 30
  treeman config set databases[0].engine mariadb
  treeman config set worktrees.links '["./.env", "./.envrc"]'`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.NArg() < 2 {
				return fmt.Errorf("usage: treeman config set <path> <value>")
			}
			path := c.Args().Get(0)
			rawValue := c.Args().Get(1)
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			p := filepath.Join(repoRoot, ".treeman.yaml")
			raw, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("read %s: %w", p, err)
			}
			var doc yaml.Node
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parse %s: %w", p, err)
			}
			segs, err := yamlpatch.ParsePath(path)
			if err != nil {
				return err
			}
			var value any
			if err := json.Unmarshal([]byte(rawValue), &value); err != nil {
				value = rawValue
			}
			newNode, err := yamlpatch.ValueToNode(value)
			if err != nil {
				return fmt.Errorf("encode value: %w", err)
			}
			prev, err := yamlpatch.Set(&doc, segs, newNode)
			if err != nil {
				return err
			}
			body, err := yamlpatch.Marshal(&doc)
			if err != nil {
				return fmt.Errorf("encode yaml: %w", err)
			}
			var validated config.Config
			if err := yaml.Unmarshal(body, &validated); err != nil {
				return fmt.Errorf("validation failed — patched file would not parse as config.Config: %w", err)
			}
			if err := atomicWriteFile(p, body); err != nil {
				return err
			}
			prevJSON := ""
			if prev != nil {
				var v any
				if err := prev.Decode(&v); err == nil {
					if b, err := json.Marshal(v); err == nil {
						prevJSON = string(b)
					}
				}
			}
			newJSON, _ := json.Marshal(value)
			if c.Bool("json") {
				return jsonStream(map[string]any{
					"path":          path,
					"previous_json": prevJSON,
					"new_json":      string(newJSON),
					"bytes":         len(body),
				})
			}
			if prevJSON == "" {
				PrintOK("set %s = %s (new key)", path, string(newJSON))
			} else {
				PrintOK("set %s: %s → %s", path, prevJSON, string(newJSON))
			}
			return nil
		},
	}
}

// openDefaultStore opens the default SQLite event store.
func openDefaultStore(ctx context.Context) (*store.Store, error) {
	p, err := store.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, p)
}

// atomicWriteFile delegates to yamlpatch.AtomicWriteWithBackup so
// every config-set / config-write path keeps the last 5 backups in
// case an agent (or human) clobbers something important.
func atomicWriteFile(path string, data []byte) error {
	return yamlpatch.AtomicWriteWithBackup(path, data, configBackupKeep)
}

// configBackupKeep is the rotation depth for `.treeman.yaml.bak.*`
// snapshots created on every config_set / config_write. Five is
// arbitrary but covers a typical session of trial-and-error edits.
const configBackupKeep = 5

// parseOlderThan accepts "24h" / "7d" / RFC3339. For durations the
// cutoff is interpreted as "older than now-d".
func parseOlderThan(s string) (time.Time, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		if d, err := time.ParseDuration(s[:len(s)-1] + "h"); err == nil {
			return time.Now().Add(-d * 24), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, f := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised older-than %q", s)
}
