# CLI reference

[← back to README](../README.md)

Auto-generated from the `treeman` binary's command tree.
Run `just sync-docs` after touching any subcommand to refresh.

## Commands

### `treeman worktree`

Aliases: `wt`

worktree lifecycle

### `treeman worktree create`

Aliases: `new`

create a worktree end-to-end

```
Creates a linked worktree, patches the env files, registers it
in SQLite, then dispatches setup hooks + prepare to the daemon. The
CLI always returns immediately — follow progress with
'treeman logs tail --follow'.

Examples:
  treeman wt create PROJ-1234
  treeman wt create feature/x --from origin/develop
  cd "$(treeman wt create feat --print-path)"
```

| Flag | Usage |
|---|---|
| `--from` | base branch |
| `--path` | explicit worktree path |
| `--repo` | repo root override |
| `--skip-hooks` |  |
| `--skip-prepare` |  |
| `--no-fetch` | skip the pre-create `git fetch origin <base>` (defaults on so new branches pick up upstream commits) |
| `--print-path` | print only the new worktree path on stdout; status lines redirect to stderr (enables `cd "$(treeman wt create …)"`) |

### `treeman worktree delete`

Aliases: `rm`

delete a worktree end-to-end

```
Runs teardown hooks + DB teardown + git worktree remove, then
removes the registry row. The teardown is dispatched to the daemon
(or a setsid child when the daemon is unreachable) — the CLI
always returns immediately.

Examples:
  treeman wt delete PROJ-1234
  treeman wt delete /path/to/wt --force      # remove stale registry entry
```

| Flag | Usage |
|---|---|
| `--repo` |  |
| `-f`, `--force` |  |
| `-y`, `--yes` | skip the confirmation prompt |
| `--detached` |  |

### `treeman worktree register`

register a worktree path (metadata only)

| Flag | Usage |
|---|---|
| `-b`, `--branch` |  |
| `--repo` |  |

### `treeman worktree unregister`

mark a worktree deleted in SQLite without touching git

### `treeman worktree list`

Aliases: `ls`

list active worktrees

| Flag | Usage |
|---|---|
| `--json` |  |
| `--with-state` | include a STATE column derived from the most recent finalize event |
| `--with-status` | include a STATUS column (clean/dirty/unpushed; forks git status + rev-list per row) |
| `--sort` | id \| mtime (HEAD commit ts) \| visited (last_visited_at) |
| `-r`, `--repo` | scope to one repo (path) |

### `treeman worktree show`

Aliases: `info`

show details, recent events, and hook runs for a worktree (defaults to the worktree containing the current directory)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--events` | number of recent events to show |
| `--hooks` | number of recent hook runs to show |
| `--no-pager` | disable the pager even when stdout is a TTY |

### `treeman worktree logs`

tail events scoped to a worktree (shorthand for `logs tail --worktree`)

| Flag | Usage |
|---|---|
| `-n` | max events to return |
| `-f`, `--follow` | stream new events as they arrive |
| `-w`, `--worktree` | filter by worktree (slug, branch, or basename) |
| `-r`, `--repo` | repo root override |
| `-A`, `--all` | show events from every worktree (defeats the cwd auto-filter) |
| `-l`, `--level` | filter by level (debug\|info\|warn\|error); repeatable |
| `-t`, `--event-type` | filter by exact event_type; repeatable |
| `-p`, `--phase` | filter by phase (precreate\|postcreate\|...); repeatable |
| `--since` | only events newer than this (e.g. 10m, 2h, 2026-05-21T00:00) |
| `--json` | emit one JSON object per line instead of the formatted columns |
| `--payload` | substring match against payload_json |
| `--hooks` | show recent hook_runs rows instead of events |
| `--no-pager` | disable the pager even when stdout is a TTY |

### `treeman worktree wait`

block until the daemon's finalize for a worktree completes

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--timeout` | give up after this duration |
| `-q`, `--quiet` | suppress progress output (still exits non-zero on failure) |

### `treeman worktree finalize`

rerun setup + prepare for a worktree (default via daemon; --local runs inline)

| Flag | Usage |
|---|---|
| `--repo` |  |
| `--local` | run setup + prepare in this process instead of dispatching to the daemon |

### `treeman worktree switch`

print the path of a worktree (for shell `cd $(…)` use)

| Flag | Usage |
|---|---|
| `--repo` | repo root override |
| `--create` | create the worktree if no match |
| `--from` | base branch (with --create) |

### `treeman worktree back`

print main repo path (with --remove, drop current worktree if clean)

| Flag | Usage |
|---|---|
| `--remove` | delete current worktree if clean + no unpushed commits |
| `--force` | with --remove: pass --force to delete |

### `treeman worktree resolve`

print the worktree path holding <branch> (registry lookup; exit nonzero on miss)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |

### `treeman worktree prev`

print previously-visited worktree (registry-tracked; cross-shell)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |

### `treeman worktree go`

switch/create branch with auto-routing (use as cd "$(treeman wt go …)")

| Flag | Usage |
|---|---|
| `--create` | treat <branch> as a new branch (falls back to checkout if it already exists) |
| `--from` | base branch (with --create) |
| `-r`, `--repo` |  |
| `--no-fetch` | skip the pre-checkout `git fetch origin <base>` |

### `treeman main`

manage main-worktree enrollment (per-branch DBs at repo root)

### `treeman main enable`

opt the repo root into the watcher lifecycle

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |

### `treeman main disable`

remove the repo root from the watcher lifecycle

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--purge` | after disabling, drop every per-branch DB the main_<branch> slug owns (current branch + every local branch). Engine resources only — the worktrees row stays soft-deleted for resurrection. |

### `treeman main status`

show main-worktree enrollment state

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman branches`

list local + remote-only branches with worktree occupancy

| Flag | Usage |
|---|---|
| `-r`, `--repo` | repo root override |
| `--json` |  |
| `--local-only` | only branches with a local ref |
| `--remote-only` | only branches that exist on origin but not locally |
| `--available` | only branches not currently occupying a live worktree |

### `treeman prepare`

ensure → dump → migrate → snapshot → replicate (foreground)

| Flag | Usage |
|---|---|
| `-w`, `--worktree` |  |
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman db`

per-worktree database operations

### `treeman db reset`

re-sync branch_scoped databases from the live base branch (defaults to the cwd's worktree)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--engine` | restrict the reset to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql accepted) |
| `--json` |  |

### `treeman db status`

show branch_scoped state: active namespace, current branch, and resumable branches (defaults to the cwd's worktree)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman sync`

fetch + advance worktrees to upstream (manual override of auto-fetch)

| Flag | Usage |
|---|---|
| `--all` | sync every registered repo |
| `--json` | emit one JSON object per repo |
| `--status` | print sync status only; do not fetch |

### `treeman hook`

run a hook phase

### `treeman hook run`

run a hook phase using the cwd's repo config

| Flag | Usage |
|---|---|
| `-w`, `--worktree` |  |
| `--json` |  |

### `treeman logs`

Aliases: `log`

query the SQLite event log

### `treeman logs tail`

print recent events, optionally streaming new ones with --follow

```
Examples:
  treeman logs tail
  treeman logs tail --follow
  treeman logs tail --worktree PROJ-1234 --level warn --level error
  treeman logs tail --since 5m --json | jq .
  treeman logs tail --event-type wt_finalize_done --event-type wt_finalize_start

When stdout is a terminal and --follow / --json are not used,
output is paged through $PAGER (default: less -FRX). Set
TREEMAN_NO_PAGER=1 to disable.
```

| Flag | Usage |
|---|---|
| `-n` | max events to return |
| `-f`, `--follow` | stream new events as they arrive |
| `-w`, `--worktree` | filter by worktree (slug, branch, or basename) |
| `-r`, `--repo` | repo root override |
| `-A`, `--all` | show events from every worktree (defeats the cwd auto-filter) |
| `-l`, `--level` | filter by level (debug\|info\|warn\|error); repeatable |
| `-t`, `--event-type` | filter by exact event_type; repeatable |
| `-p`, `--phase` | filter by phase (precreate\|postcreate\|...); repeatable |
| `--since` | only events newer than this (e.g. 10m, 2h, 2026-05-21T00:00) |
| `--json` | emit one JSON object per line instead of the formatted columns |
| `--payload` | substring match against payload_json |
| `--no-pager` | disable the pager even when stdout is a TTY |

### `treeman logs grep`

search events whose message (or --payload) matches a pattern

```
Examples:
  treeman logs grep "snapshot cache"
  treeman logs grep "^prepare" --regex
  treeman logs grep checksum --search-payload --level info
```

| Flag | Usage |
|---|---|
| `-n` | max events to return |
| `-f`, `--follow` | stream new events as they arrive |
| `-w`, `--worktree` | filter by worktree (slug, branch, or basename) |
| `-r`, `--repo` | repo root override |
| `-A`, `--all` | show events from every worktree (defeats the cwd auto-filter) |
| `-l`, `--level` | filter by level (debug\|info\|warn\|error); repeatable |
| `-t`, `--event-type` | filter by exact event_type; repeatable |
| `-p`, `--phase` | filter by phase (precreate\|postcreate\|...); repeatable |
| `--since` | only events newer than this (e.g. 10m, 2h, 2026-05-21T00:00) |
| `--json` | emit one JSON object per line instead of the formatted columns |
| `--payload` | substring match against payload_json |
| `-e`, `--regex` | treat the pattern as a Go regexp instead of a substring |
| `--search-payload` | search the payload_json column instead of the message |
| `--no-pager` | disable the pager even when stdout is a TTY |

### `treeman logs hooks`

show recent hook_runs (precreate/postcreate/predelete/postdelete) for a worktree

```
Examples:
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
included, so the original terminal colors round-trip.
```

| Flag | Usage |
|---|---|
| `-n` | max rows |
| `-r`, `--repo` | repo root override |
| `-A`, `--all` | show hook runs from every worktree (skips cwd auto-resolve) |
| `--show` | render captured stdout+stderr for the given hook_run id |
| `--json` |  |

### `treeman logs purge`

delete event-log rows by filter (at least one filter required)

```
At least one filter must be supplied so an unfiltered call can
never wipe the whole table.

  treeman logs purge --older-than 30d
  treeman logs purge --worktree PROJ-1234
  treeman logs purge --level debug --older-than 7d
```

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `-w`, `--worktree` |  |
| `--older-than` | duration (24h, 7d) or RFC3339 cutoff |
| `-l`, `--level` |  |
| `-t`, `--event-type` |  |
| `--json` |  |

### `treeman config`

config helpers

### `treeman config validate`

validate the config loads without error

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman config show`

dump the resolved config

| Flag | Usage |
|---|---|
| `--resolved` |  |
| `--no-pager` | disable the pager even when stdout is a TTY |
| `--json` |  |

### `treeman config set`

patch a single field of .treeman.yaml by dotted path (preserves comments + key order)

```
<value> is parsed as JSON when possible (so 30, true, "x", ["a","b"] all
work) and falls back to a literal string otherwise.

Examples:
  treeman config set daemon.gc_interval 30
  treeman config set databases[0].engine mariadb
  treeman config set worktrees.links '["./.env", "./.envrc"]'
```

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman schema`

JSON schema helpers

### `treeman schema dump`

| Flag | Usage |
|---|---|
| `--out` |  |

### `treeman schema install`

| Flag | Usage |
|---|---|
| `--global` | write to the user-global path instead of <repo>/schemas/ |
| `--url` | skip the file write; point the modeline at the upstream URL |

### `treeman daemon`

daemon lifecycle

### `treeman daemon start`

### `treeman daemon stop`

### `treeman daemon reload`

ask the daemon to re-read config + restart watchers (no process restart)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | limit reload to one repo path; defaults to all |

### `treeman daemon status`

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman daemon install`

### `treeman daemon uninstall`

| Flag | Usage |
|---|---|
| `-y`, `--yes` | skip the confirmation prompt |

### `treeman fw`

Aliases: `frameworks`

framework detection

### `treeman fw detect`

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman slug`

print the slug treeman derives for a worktree

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman init`

| Flag | Usage |
|---|---|
| `--force` |  |
| `--json` |  |

### `treeman doctor`

health-check the local treeman setup

| Flag | Usage |
|---|---|
| `--json` | emit one JSON line per check |
| `--fix` | auto-apply remediations for `schema` (install) and `registry` (repair) checks; re-runs the probe so the printed result reflects the post-fix state |

### `treeman registry`

SQLite worktree-registry maintenance

### `treeman registry repair`

reconcile the SQLite registry with `git worktree list` (register drift / mark missing)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman registry remove`

drop a repo from the SQLite registry (stops watchers, removes tracking rows; leaves external state alone)

```
Refuses by default when any worktree row under the repo is still
active (deleted_at IS NULL) — that almost always means running
services, on-disk checkouts, or per-worktree databases. Pass --force
to remove anyway; this only deletes registry rows and never destroys
external resources.

Examples:
  treeman registry remove --repo /abs/path
  treeman registry remove --repo /abs/path --force
  treeman registry remove --repo /abs/path --yes
```

| Flag | Usage |
|---|---|
| `-r`, `--repo` | repo path; defaults to current cwd's repo root |
| `-f`, `--force` | remove even when active worktrees exist |
| `-y`, `--yes` | skip the confirmation prompt |
| `--json` |  |

### `treeman snapshots`

Aliases: `snap`

snapshot cache visibility + purge

### `treeman snapshots list`

list cached snapshots (template DBs) for this repo

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman snapshots purge`

drop every cached snapshot for this repo and force the next prepare to rebuild

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman mcp`

run the Model Context Protocol server (stdio transport)

