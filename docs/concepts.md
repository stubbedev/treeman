# Concepts

## Worktrees and the registry

treeman manages **linked worktrees**: the extra checkouts `git worktree
add` creates, by default under `.worktrees/` in the repository. Every
worktree treeman creates or adopts gets a row in its SQLite registry,
which also holds the event log, the snapshot table and port
allocations. `treeman worktree register` adopts a worktree created
without treeman.

The main checkout is left alone unless you opt it in; see
[the main worktree](#the-main-worktree).

## Slugs

Every worktree has a **slug**, the identifier all of its per-worktree
names are rendered from. It is derived, never stored by hand:

| Worktree | Slug | Example |
|---|---|---|
| directory name contains a ticket key (`[A-Z]+-\d+`) | ticket + path tag | `proj_123_33a85ab4` |
| anything else | path tag | `wt_33a85ab4` |
| main checkout (when enrolled) | branch + branch tag | `main_develop_1ed51c85` |

The tag is a hash of the worktree's path (or, for the main checkout, of
the branch name), so two worktrees never share a slug.
`TREEMAN_SLUG_SUFFIX` folds an extra value into every tag, which keeps
concurrent CI jobs apart. `treeman slug [path]` prints the slug for a
path.

Config fields such as `name_template` reference the slug through
[template tokens](configuration.md#templated-names):
`myapp_testing_{slug}` becomes `myapp_testing_proj_123_33a85ab4`.

## Namespaces per engine

Each `databases:` entry gets its own namespace per worktree. treeman
talks to the engines over their wire protocols (Go `database/sql`, the
official MongoDB, Redis and Elasticsearch SDKs); it never shells out to
`mysql`, `psql`, `mongosh` or `redis-cli`.

| Engine | Aliases | Isolation |
|---|---|---|
| MySQL | `mariadb`, `tidb` | separate database |
| PostgreSQL | `postgresql` | separate database |
| MongoDB | | separate database |
| Redis | `valkey`, `dragonfly` | key prefix in DB 0 |
| Elasticsearch | `opensearch` | index-name prefix |
| S3-compatible storage | | per-worktree bucket (lifecycle only) |
| SQLite | `duckdb` | separate file |

## The prepare pipeline

`prepare` brings a worktree's databases up to date. For each entry:

1. **ensure**: create the namespace if it is missing.
2. **dump**: load the configured seed dump.
3. **migrate**: run your migrate (and seed) command against it, with
   env overrides pointing the command at the per-run database.
4. **snapshot**: keep the result as a cached template.
5. **replicate**: clone the template into the worktree's database and
   its parallel-test clones.

Steps 2–4 are skipped on a cache hit. The snapshot key is a content
fingerprint of the engine and its version, the dump, and every file
listed under `inputs:` (migrations, lockfiles). It does not include the
branch or the database name, so every worktree whose inputs match
reuses one template, and creating the tenth worktree of a branch costs a
clone, not a migration run. Changing an input moves the fingerprint and
the next prepare rebuilds. See [snapshots](snapshots.md) for the cache
and its retention rules.

## Lifecycle

`treeman worktree create` checks the branch out, applies `copies`,
`links` and `patches`, registers the worktree, then hands the rest to
the daemon:

1. `create-before-engines` hooks (dependency installs)
2. prepare
3. `create-after-engines` hooks (anything that boots the app)

Deletion mirrors it: `delete-before-engines` hooks, database drops,
`delete-after-engines` hooks, `git worktree remove`. `checkout` hooks
fire on a branch switch and `file-change` hooks when a watched input
changes. Actions in one hook list run in parallel.

`treeman status`, `treeman worktree list` and the MCP status tool report
each worktree as one of:

| State | Meaning |
|---|---|
| `preparing` | create is running |
| `ready` | the last create or prepare succeeded |
| `deferred` | paused while a merge, rebase, cherry-pick or revert is in progress; resumes when it finishes |
| `error` | the last create, prepare or teardown failed; retry with `treeman worktree finalize` |
| `teardown` | delete is running |

Every step writes events to the log. `treeman logs tail --follow` shows
them live, `treeman worktree show <name>` summarises one worktree, and
[events](events.md) lists every event type.

## The daemon

`treemand` is a per-user daemon that runs hooks and prepares, watches
inputs with fsnotify, fetches upstream on a timer (`auto_fetch`),
sweeps old snapshots and logs, and serialises work per worktree. The
`treeman` CLI and the MCP server talk to it over a unix socket that only
the owning user can connect to.

When the daemon is unreachable, commands fall back to running the same
work in-process, so nothing is stranded. `--require-daemon` (or
`TREEMAN_REQUIRE_DAEMON=1`) turns that fallback into an error. CI uses
the in-process path on purpose: see [CI without a daemon](ci.md).
[Internals](internals.md) covers the socket, RPC and storage layout.

## Configuration layers

Config comes from `~/.config/treeman/config.yaml` (machine-wide) and
`.treeman.yaml` in the repository (committed), plus an optional
gitignored `.treeman.local.yaml`. Every key has a scope that decides
which file it may live in, and a key in the wrong file is a load error.
See [configuration](configuration.md).

## The main worktree

By default the repository's own checkout is not managed. With
`main_worktree.enabled: true` (or `treeman main enable`), it joins the
same lifecycle: each branch checked out at the repo root gets its own
databases under a `main_<branch>_<tag>` slug, and the hooks fire there
too. See [main worktree](configuration.md#main-worktree).
