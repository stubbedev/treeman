# CLI reference

[← back to README](../README.md)

Auto-generated from the `treeman` binary's command tree.
Run `just sync-docs` after touching any subcommand to refresh.

## Commands

### `treeman worktree`

worktree lifecycle

### `treeman worktree create`

create a worktree end-to-end

```
Creates a linked worktree, patches the env files, registers it
in SQLite, then dispatches setup hooks + prepare to the daemon. The
CLI always returns immediately — follow progress with
'treeman logs tail --follow'.

Examples:
  treeman worktree create PROJ-1234
  treeman worktree create feature/x --from origin/develop
  cd "$(treeman worktree create feat --print-path)"
```

| Flag | Usage |
|---|---|
| `--from` | base branch |
| `--path` | explicit worktree path |
| `-r`, `--repo` | repo root override |
| `--skip-hooks` | checkout only: no create hooks, no databases (links/copies/patches still applied inline) |
| `--skip-prepare` | run hooks/links/copies/patches but provision no databases |
| `--no-fetch` | skip the pre-create `git fetch origin <base>` (defaults on so new branches pick up upstream commits) |
| `--print-path` | print only the new worktree path on stdout; status lines redirect to stderr (enables `cd "$(treeman worktree create …)"`) |

### `treeman worktree delete`

delete a worktree end-to-end

```
Runs teardown hooks + DB teardown + git worktree remove, then
removes the registry row. The teardown is dispatched to the daemon
over the RPC socket — the CLI returns immediately. If the daemon
can't be reached (even after autostart), the teardown runs
in-process instead (blocks until done).

Several targets may be given, and the no-argument picker is a
Tab-toggle multi-select — both delete every named worktree in one
invocation.

Examples:
  treeman worktree delete PROJ-1234
  treeman worktree delete PROJ-1234 PROJ-5678       # several at once
  treeman worktree delete /path/to/wt --force      # remove stale registry entry
```

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `-f`, `--force` |  |
| `-y`, `--yes` | skip the confirmation prompt |
| `--detached` |  |

### `treeman worktree register`

register a worktree path (metadata only)

| Flag | Usage |
|---|---|
| `-b`, `--branch` |  |
| `-r`, `--repo` |  |

### `treeman worktree unregister`

mark a worktree deleted in SQLite without touching git

### `treeman worktree list`

list active worktrees

| Flag | Usage |
|---|---|
| `--json` |  |
| `--tsv` | machine output: one <path>\t<branch> line per active worktree (for shell consumption) |
| `--with-state` | include a STATE column derived from the most recent finalize event |
| `--with-status` | include a STATUS column (clean/dirty/unpushed; forks git status + rev-list per row) |
| `--sort` | id \| mtime (HEAD commit ts) \| visited (last_visited_at) |
| `-r`, `--repo` | scope to one repo (path) |

### `treeman worktree show`

show details, recent events, and hook runs for a worktree (defaults to the worktree containing the current directory)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--events` | number of recent events to show |
| `--hooks` | number of recent hook runs to show |
| `--no-pager` | disable the pager even when stdout is a TTY |
| `--json` |  |

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
| `-r`, `--repo` |  |
| `--local` | run setup + prepare in this process instead of dispatching to the daemon |

### `treeman worktree back`

print main repo path (with --remove, drop current worktree if clean)

| Flag | Usage |
|---|---|
| `--remove` | delete current worktree if clean + no unpushed commits |
| `--force` | with --remove: pass --force to delete |

### `treeman worktree prev`

print previously-visited worktree (registry-tracked; cross-shell)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |

### `treeman worktree go`

resolve/create/checkout a worktree by name or branch (use as cd "$(treeman worktree go …)")

| Flag | Usage |
|---|---|
| `--create` | create the worktree if nothing matches |
| `--checkout` | git checkout the branch (auto-routes main vs new worktree) instead of pure path resolution |
| `--from` | base branch (with --create/--checkout) |
| `-r`, `--repo` |  |
| `--no-fetch` | skip the pre-checkout `git fetch origin <base>` |

### `treeman worktree switch`

switch to or create a branch's worktree (prints dest path for cd)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--from` | base branch when creating |
| `--no-fetch` | skip the pre-checkout fetch |

### `treeman worktree prune`

prune worktrees whose directory is gone (git worktree prune + registry reconcile)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |

### `treeman git`

git workflow helpers (commit, push, add, diff, log, stash, switch)

### `treeman git commit`

commit with auto ticket prefix (trailing \ opens editor)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git push`

push -u origin <branch> (guards protected branches + divergence)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git add`

interactively stage files (M→patch, D→removal, A→untracked)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git diff`

working-tree diff, or three-dot diff vs a branch (--pick, --patch)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `--pick` | pick the comparison branch interactively |
| `--patch` | write the diff to <cur>--<target>.diff instead of paging |

### `treeman git status`

short status

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git pull`

pull current branch (--all, or --pick an origin branch)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `-a`, `--all` | pull all branches |
| `--pick` | pull a selected origin branch |

### `treeman git fetch`

fetch (--all for all remotes)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `-a`, `--all` |  |

### `treeman git stash`

stash all local changes

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git stash pop`

pop a selected stash

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git stash clear`

drop the entire stash stack (with confirmation)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git wipe`

wipe local changes (stash + drop; --all also clears the stash stack)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `-a`, `--all` |  |

### `treeman git log`

interactive log (Enter: show, Ctrl+X: cherry-pick, Ctrl+R: revert, Ctrl+Y: copy hash)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `-n`, `--limit` | number of commits to load |

### `treeman git switch`

checkout or create a branch, worktree-aware (prints dest path for cd)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--from` | base branch when creating |
| `--no-fetch` | skip the pre-checkout fetch |

### `treeman git amend`

amend the last commit (--no-edit keeps its message)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `--no-edit` | keep the existing message; just fold in staged changes |

### `treeman git undo`

uncommit the last commit, keeping its changes staged (reset --soft HEAD~1)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git discard`

discard working-tree changes on selected files (irreversible)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git branch-delete`

delete local branches (picker; merged branches marked)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |

### `treeman git sync-branch`

update the current branch from its base (fetch + merge/rebase base in)

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `--base` | base branch (default: repo default branch) |
| `--rebase` | rebase onto the base instead of merging |

### `treeman git fixup`

commit staged changes as a fixup! of a picked commit, then autosquash

| Flag | Usage |
|---|---|
| `-r`, `--repo` | worktree/repo dir (default: cwd) |
| `--no-rebase` | create the fixup! commit but skip the autosquash rebase |

### `treeman repos`

list repos enrolled in treeman

```
Reads the SQLite registry directly (no daemon round-trip), so it
works while the daemon is down. A repo is enrolled the first time
treeman touches it (wt create, prepare, or daemon watch); drop one
with `treeman registry remove`.
```

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman status`

summarize worktree health across all repos (bar/waybar widget)

```
Aggregates every active worktree across all registered repos into
four buckets — stable (ready), up (preparing), down (tearing down),
failed (last finalize errored) — and renders them.

Formats (--format):
  icon    one-line counter (default), plain text
  hover   per-repo grouped detail, plain text (the "cal-style" block)
  waybar  {"text","tooltip","class"} JSON for a waybar custom module
  json    the raw aggregated shape (counts + per-repo worktrees)
  <name>  a custom single-line {key} format from status.formats

Icons, labels, separators, the hover header/row templates, and custom
formats are all configured under the global config's status: block.
```

| Flag | Usage |
|---|---|
| `-f`, `--format` | icon \| hover \| waybar \| json \| <name from status.formats> |

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
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

### `treeman db`

per-worktree database operations

### `treeman db reset`

re-sync branch_scoped databases from the live base branch (defaults to the cwd's worktree)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--engine` | restrict the reset to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql/valkey/dragonfly accepted) |
| `--json` |  |
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

### `treeman db save`

capture branch_scoped databases into the current branch's durable copies without switching (defaults to the cwd's worktree)

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--engine` | restrict the save to one engine family (mysql, postgres, mongodb, redis, elasticsearch; aliases like mariadb/postgresql/valkey/dragonfly accepted) |
| `--json` |  |
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

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
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

### `treeman logs`

query the SQLite event log

### `treeman logs tail`

print recent events, optionally streaming new ones with --follow

```
Examples:
  treeman logs tail
  treeman logs tail --follow
  treeman logs tail --worktree PROJ-1234 --level warn --level error
  treeman logs tail --since 5m --json | jq .
  treeman logs tail --event-type worktree:create:end --event-type worktree:create:start

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

### `treeman config history`

list stored .treeman.yaml generations for this repo

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman config restore`

write a stored generation back to .treeman.yaml

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
| `--scope` | full (default, every key) \| global (~/.config/treeman/config.yaml keys) \| repo (.treeman.yaml keys) |

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

### `treeman daemon state`

live runtime snapshot — watchers, in-flight finalizes/teardowns, auto-fetch backoffs (CLI twin of the MCP daemon_state tool)

| Flag | Usage |
|---|---|
| `--json` |  |

### `treeman daemon install`

### `treeman daemon uninstall`

| Flag | Usage |
|---|---|
| `-y`, `--yes` | skip the confirmation prompt |

### `treeman frameworks`

framework detection

### `treeman frameworks detect`

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
| `--global` | scaffold the user-global ~/.config/treeman/config.yaml (machine-wide defaults) instead of a per-repo .treeman.yaml |

### `treeman doctor`

health-check the local treeman setup

| Flag | Usage |
|---|---|
| `--json` | emit one JSON line per check |
| `--fix` | auto-apply remediations for `schema` (install) and `registry` (repair) checks; re-runs the probe so the printed result reflects the post-fix state |

### `treeman registry`

SQLite worktree-registry maintenance

### `treeman registry list`

Aliases: `ls`

list repos enrolled in the registry

```
Reads the SQLite registry directly (no daemon round-trip), so it
works while the daemon is down. A repo is enrolled the first time
treeman touches it (wt create, prepare, or daemon watch); drop one
with `treeman registry remove`.
```

| Flag | Usage |
|---|---|
| `--json` |  |

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

snapshot cache visibility + purge

### `treeman snapshots list`

list cached snapshots (template DBs) for this repo

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |

### `treeman snapshots prune`

delete snapshot rows whose engine-side template no longer exists (orphans); live templates untouched

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

### `treeman snapshots purge`

drop every cached snapshot for this repo and force the next prepare to rebuild

| Flag | Usage |
|---|---|
| `-r`, `--repo` |  |
| `--json` |  |
| `-f`, `--wait`, `--foreground` | stream the daemon's live progress and block until done (default: dispatch and return) |

### `treeman mcp`

run the Model Context Protocol server (stdio, or --http for a shared HTTP daemon)

### `treeman notify`

desktop notification helpers

### `treeman notify test`

send a sample desktop notification to verify the backend

```
Fires a single test banner through the configured (or
auto-detected) notification backend, so you can confirm notify-send
(Linux) / osascript (macOS) actually shows a notification before
enabling notifications: in your config.

Works regardless of notifications.enabled — it tests the transport, not
the opt-in. Use --backend to probe a specific sender.
```

| Flag | Usage |
|---|---|
| `-b`, `--backend` | auto \| notify-send \| osascript \| none (default: notifications.backend, else auto) |

