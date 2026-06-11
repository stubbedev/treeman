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
	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/rpc"
	"github.com/stubbedev/treeman/internal/snapshot"
	"github.com/stubbedev/treeman/internal/store"
	"github.com/stubbedev/treeman/internal/ui"
	"github.com/stubbedev/treeman/internal/wtreg"
	"github.com/stubbedev/treeman/internal/yamlpatch"
)

// RegistryCmd — `treeman registry {repair, remove}` exposes the
// SQLite-side reconciliation primitives that doctor's `registry`
// check reports. (Per-worktree register/unregister already live
// under `treeman wt`.)
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
					task := rpc.Task{Type: rpc.TaskRegistryRepair, RepoPath: repoRoot}
					if c.Bool("json") {
						return runResultJSON(ctx, task)
					}
					payload, err := resultPayload(ctx, task)
					if err != nil {
						return err
					}
					var res wtreg.RepairResult
					_ = json.Unmarshal(payload, &res)
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
			registryRemove(),
		},
	}
}

// registryRemove implements `treeman registry remove --repo R` —
// drops the repo from the SQLite registry plus every child row
// (worktrees, events, snapshots, hook_runs). Does
// NOT touch databases, on-disk worktree directories, or dump caches.
//
// When the daemon is running, the work is delegated via RPC so live
// watchers are stopped in the same process; otherwise we fall back to
// direct SQLite manipulation and any stale watchers will be GC'd at
// the next daemon restart.
func registryRemove() *cli.Command {
	return &cli.Command{
		Name:  "remove",
		Usage: "drop a repo from the SQLite registry (stops watchers, removes tracking rows; leaves external state alone)",
		Description: `Refuses by default when any worktree row under the repo is still
active (deleted_at IS NULL) — that almost always means running
services, on-disk checkouts, or per-worktree databases. Pass --force
to remove anyway; this only deletes registry rows and never destroys
external resources.

Examples:
  treeman registry remove --repo /abs/path
  treeman registry remove --repo /abs/path --force
  treeman registry remove --repo /abs/path --yes`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Aliases: []string{"r"}, Usage: "repo path; defaults to current cwd's repo root"},
			&cli.BoolFlag{Name: "force", Aliases: []string{"f"}, Usage: "remove even when active worktrees exist"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
			&cli.BoolFlag{Name: "json"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			force := c.Bool("force")
			if !c.Bool("yes") && !c.Bool("json") {
				if !ui.Confirm(fmt.Sprintf("remove %s from the treeman registry?", repoRoot)) {
					return errors.New("aborted")
				}
			}

			// Daemon path first — preferred so live watchers stop in
			// the same process. Falls back to direct SQLite when the
			// daemon socket isn't there.
			if _, dialErr := os.Stat(mustSocketPath()); dialErr == nil {
				resp, err := rpcCall(ctx, repoRoot, force)
				if err == nil {
					if resp.Kind == rpc.KindError {
						return fmt.Errorf("daemon: %s", resp.Message)
					}
					if c.Bool("json") {
						return jsonStream(map[string]any{"repo": repoRoot, "removed": true, "via": "daemon"})
					}
					PrintOK("removed %s from registry (via daemon)", repoRoot)
					return nil
				}
				PrintWarn("daemon RPC failed (%v) — falling back to direct SQLite", err)
			}

			st, err := openDefaultStore(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			repoID, err := st.LookupRepoID(ctx, repoRoot)
			if err != nil {
				return err
			}
			if repoID == 0 {
				return fmt.Errorf("repo not enrolled: %s", repoRoot)
			}
			if !force {
				n, err := st.CountActiveWorktreesForRepo(ctx, repoID)
				if err != nil {
					return err
				}
				if n > 0 {
					return fmt.Errorf("repo has %d active worktree(s); run `treeman wt delete` first or re-run with --force", n)
				}
			}
			if err := st.RemoveRepo(ctx, repoID); err != nil {
				return err
			}
			if c.Bool("json") {
				return jsonStream(map[string]any{"repo": repoRoot, "removed": true, "via": "sqlite"})
			}
			PrintOK("removed %s from registry (direct SQLite)", repoRoot)
			return nil
		},
	}
}

func mustSocketPath() string {
	p, _ := rpc.SocketPath()
	return p
}

func rpcCall(ctx context.Context, repoPath string, force bool) (rpc.Response, error) {
	return rpc.Call(ctx, rpc.Request{
		Method:     rpc.MethodRepoRemove,
		RepoRemove: &rpc.RepoRemoveArgs{RepoPath: repoPath, Force: force},
	})
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
					defer func() { _ = st.Close() }()
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
					// Pre-warmed spare pools are engine-side only (no
					// SQLite rows) — count them here so `prewarm` users
					// can see their pools without psql. Best-effort: an
					// unreachable engine just blanks the column.
					spares := map[string]int{}
					if cfg, lerr := resolve.LoadResolved(repoRoot); lerr == nil {
						if sc, serr := snapshot.SpareCounts(ctx, &cfg); serr == nil {
							spares = sc
						}
					}
					if c.Bool("json") {
						type snapRow struct {
							store.SnapshotEvictionCandidate
							Spares int `json:"spares"`
						}
						rows := make([]snapRow, 0, len(cands))
						for _, sc := range cands {
							rows = append(rows, snapRow{sc, spares[sc.TemplateName]})
						}
						return jsonStream(map[string]any{"repo": repoRoot, "snapshots": rows})
					}
					if len(cands) == 0 {
						PrintInfo("no snapshots cached for %s", repoRoot)
						return nil
					}
					t := ui.NewTable("ENGINE", "TEMPLATE", "SOURCE_DB", "SPARES", "FINGERPRINT")
					for _, sc := range cands {
						spareCol := "-"
						if n, ok := spares[sc.TemplateName]; ok {
							spareCol = strconv.Itoa(n)
						}
						t.Row(ui.Cyan(sc.Engine), sc.TemplateName, sc.SourceDB, spareCol, ui.Dim(sc.Fingerprint))
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
					&cli.BoolFlag{
						Name:    "foreground",
						Aliases: []string{"wait", "f"},
						Usage:   "stream the daemon's live progress and block until done (default: dispatch and return)",
					},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					repoRoot, err := resolveRepo(c.String("repo"))
					if err != nil {
						return err
					}
					task := rpc.Task{
						Type: rpc.TaskSnapshotsPurge, RepoPath: repoRoot,
						InheritedEnv: CaptureInheritedEnv(),
					}
					if c.Bool("json") {
						return runResultJSON(ctx, task)
					}
					return dispatchTask(ctx, task, c.Bool("foreground"), "snapshots purge")
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
				return errors.New("at least one filter (--repo, --worktree, --older-than, --level, --event-type) is required")
			}
			// Build the purge filter as task params; the daemon owns the
			// DELETE (single SQLite writer). Scope paths are resolved to
			// absolute here — the daemon's cwd is not the user's.
			params := map[string]string{}
			if lv := validateLevels(c.StringSlice("level")); len(lv) > 0 {
				params["levels"] = strings.Join(lv, ",")
			}
			if et := c.StringSlice("event-type"); len(et) > 0 {
				params["event_types"] = strings.Join(et, ",")
			}
			if v := c.String("older-than"); v != "" {
				t, err := parseOlderThan(v)
				if err != nil {
					return err
				}
				params["until_ms"] = strconv.FormatInt(t.UnixMilli(), 10)
			}
			if r := c.String("repo"); r != "" {
				repoAbs, err := resolveRepo(r)
				if err != nil {
					return err
				}
				params["repo"] = repoAbs
			}
			if w := c.String("worktree"); w != "" {
				params["worktree"] = w
			}
			task := rpc.Task{Type: rpc.TaskLogsPurge, Params: params}
			if c.Bool("json") {
				return runResultJSON(ctx, task)
			}
			payload, err := resultPayload(ctx, task)
			if err != nil {
				return err
			}
			var r struct {
				RowsRemoved int64 `json:"rows_removed"`
			}
			_ = json.Unmarshal(payload, &r)
			PrintOK("removed %d event row(s)", r.RowsRemoved)
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
				return errors.New("usage: treeman config set <path> <value>")
			}
			path := c.Args().Get(0)
			rawValue := c.Args().Get(1)
			repoRoot, err := resolveRepo(c.String("repo"))
			if err != nil {
				return err
			}
			p := filepath.Join(repoRoot, ".treeman.yaml")
			body, prev, value, err := applyConfigSet(p, path, rawValue)
			if err != nil {
				return err
			}
			if err := writeConfig(ctx, repoRoot, p, body); err != nil {
				return err
			}
			prevJSON := decodePrevJSON(prev)
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

// applyConfigSet reads .treeman.yaml at p, patches the dotted path with
// rawValue (parsed as JSON when possible, literal string otherwise),
// and validates the result still parses as config.Config. Returns the
// new body, the previous node at that path (nil for a new key), and the
// parsed value.
func applyConfigSet(p, path, rawValue string) (body []byte, prev *yaml.Node, value any, err error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read %s: %w", p, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, nil, fmt.Errorf("parse %s: %w", p, err)
	}
	segs, err := yamlpatch.ParsePath(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(segs) > 0 && segs[0].Key != "" {
		if err := config.CheckKeyInLayer(segs[0].Key, "repo"); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := json.Unmarshal([]byte(rawValue), &value); err != nil {
		value = rawValue
	}
	newNode, err := yamlpatch.ValueToNode(value)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode value: %w", err)
	}
	prev, err = yamlpatch.Set(&doc, segs, newNode)
	if err != nil {
		return nil, nil, nil, err
	}
	body, err = yamlpatch.Marshal(&doc)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode yaml: %w", err)
	}
	var validated config.Config
	if err := yaml.Unmarshal(body, &validated); err != nil {
		return nil, nil, nil, fmt.Errorf("validation failed — patched file would not parse as config.Config: %w", err)
	}
	return body, prev, value, nil
}

// decodePrevJSON renders a patched-over node as compact JSON for the
// "old → new" diff line. Returns "" for a new key or any decode failure.
func decodePrevJSON(prev *yaml.Node) string {
	if prev == nil {
		return ""
	}
	var v any
	if prev.Decode(&v) != nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// openDefaultStore opens the default SQLite event store.
func openDefaultStore(ctx context.Context) (*store.Store, error) {
	p, err := store.DefaultDBPath()
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, p)
}

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
