# treeman

**Isolated databases for every git worktree.**

A git worktree gives each branch its own checkout, but the checkout
still points at the same development database. treeman gives every
worktree its own databases, search indices and parallel test clones,
migrated and ready when the worktree is, kept current as migrations
change, and dropped when the worktree is deleted.

```sh
treeman worktree create PROJ-123   # checkout + databases + hooks
treeman worktree delete PROJ-123   # hooks + drop databases + remove
```

It works with any language or framework: you declare the migrate
command, and treeman runs it against the right database.

## Highlights

- **MySQL, PostgreSQL, MongoDB, Redis, Elasticsearch, S3, SQLite**, plus
  MariaDB, TiDB, Valkey, DragonflyDB, OpenSearch and DuckDB, over their
  wire protocols. No `mysql` or `psql` binaries needed.
- **Snapshot cache.** Migrated templates are keyed by the content of
  your migrations, dumps and lockfiles, so new worktrees are cloned
  instead of migrated. Postgres can keep pre-warmed spares that are
  claimed in milliseconds regardless of size.
- **Parallel test clones** for paratest, pytest-xdist, jest, vitest,
  cargo-nextest and others: one clone per worker.
- **Hooks** for create, delete, checkout and file changes, with
  parallel actions and per-step logs.
- **A file watcher** that re-prepares when inputs change, and auto-fetch
  that keeps worktrees current with upstream.
- **Git workflow helpers** with a built-in picker: commit, push, switch,
  log, stash, fixup.
- **An MCP server** so Claude Code, Claude Desktop and Cursor can
  inspect and drive it.
- **One static binary** per platform, plus a user-mode daemon.

## Where to start

| If you want to… | Read |
|---|---|
| install treeman and create your first worktree | [Getting started](getting-started.md) |
| understand slugs, the prepare pipeline and the daemon | [Concepts](concepts.md) |
| write a `.treeman.yaml` | [Configuration](configuration.md) |
| prepare databases in CI | [CI without a daemon](ci.md) |
| connect an AI agent | [AI integration](mcp.md) |
| look up a command, key or event | the **Reference** section |

treeman is dual-licensed under Apache-2.0 or MIT. Source and issues:
[github.com/stubbedev/treeman](https://github.com/stubbedev/treeman).
