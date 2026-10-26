# treeman

**Isolated databases for every git worktree.**

treeman gives each git worktree its own databases, search indices and
parallel test clones: migrated and ready when the worktree is, kept
current as migrations change, dropped when the worktree is deleted.
Language- and framework-agnostic; you declare the migrate command.

**Documentation: [stubbedev.github.io/treeman](https://stubbedev.github.io/treeman/)**

```sh
treeman worktree create PROJ-123   # checkout + databases + hooks
treeman worktree delete PROJ-123   # hooks + drop databases + remove
```

## Features

- MySQL, PostgreSQL, MongoDB, Redis, Elasticsearch, S3 and SQLite (plus
  MariaDB, TiDB, Valkey, DragonflyDB, OpenSearch, DuckDB), spoken over
  their wire protocols.
- A snapshot cache keyed by the content of your migrations, dumps and
  lockfiles: new worktrees are cloned, not re-migrated.
- One database clone per parallel test worker (paratest, pytest-xdist,
  jest, vitest, cargo-nextest, …).
- Create, delete, checkout and file-change hooks; a file watcher and
  auto-fetch that keep worktrees current.
- Git helpers with a built-in picker, an MCP server for AI agents, and a
  daemon-less mode for CI.
- One static binary per platform plus a user-mode daemon.

## Install

```sh
brew install stubbedev/tap/treeman          # Homebrew (macOS, Linux)
nix profile install github:stubbedev/treeman # Nix
composer require --dev stubbedev/treeman     # Composer (PHP projects)
```

Release tarballs for linux amd64/arm64 and darwin arm64 are on the
[releases page](https://github.com/stubbedev/treeman/releases). See
[Getting started](https://stubbedev.github.io/treeman/getting-started/)
for every option.

## Quick start

```sh
cd ~/code/my-app
treeman init                 # write a .treeman.yaml for this repo
treeman daemon install       # start the daemon now and at login
treeman worktree create PROJ-123
cd "$(treeman worktree go PROJ-123)"
```

## Documentation

- [Getting started](https://stubbedev.github.io/treeman/getting-started/)
- [Concepts](https://stubbedev.github.io/treeman/concepts/)
- [Configuration](https://stubbedev.github.io/treeman/configuration/)
- [CI without a daemon](https://stubbedev.github.io/treeman/ci/)
- [AI integration (MCP)](https://stubbedev.github.io/treeman/mcp/)
- [CLI reference](https://stubbedev.github.io/treeman/cli/)

The site is built from [`docs/`](docs/) and redeployed on every push to
`master`. Preview it locally with `nix develop --command just docs-serve`.

## License

Dual-licensed under Apache-2.0 OR MIT.
