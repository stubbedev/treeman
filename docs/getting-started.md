# Getting started

## Requirements

- **git**: `git worktree` is what treeman manages.
- **Your database servers.** treeman manages namespaces on existing
  MySQL, PostgreSQL, MongoDB, Redis, Elasticsearch or S3 servers; it
  does not run them. Local, remote and container-hosted servers all
  work (containers are discovered via `docker inspect` / `compose ps`).
  SQLite and DuckDB need no server.
- **systemd `--user` (Linux) or launchd (macOS)** to start the daemon at
  login. Without either, `treeman daemon start` still works, but the
  daemon won't survive a reboot.

treeman is one static binary per platform (plus the `treemand`
daemon): no CGo, no system libraries, no language runtime. Linux is
the primary platform; macOS is covered by CI too.

## Install

=== "Homebrew"

    ```sh
    brew install stubbedev/tap/treeman
    ```

=== "Nix"

    ```sh
    nix profile install github:stubbedev/treeman
    ```

=== "Release tarball"

    ```sh
    # substitute linux-arm64 or darwin-arm64 as needed
    VER=$(curl -fsSL https://api.github.com/repos/stubbedev/treeman/releases/latest \
      | grep '"tag_name":' | cut -d'"' -f4 | sed 's/^v//')
    curl -fL -o /tmp/treeman.tgz \
      "https://github.com/stubbedev/treeman/releases/download/v${VER}/treeman-${VER}-linux-amd64.tar.gz"
    tar -xzf /tmp/treeman.tgz -C /tmp
    install /tmp/treeman-*-linux-amd64/treeman /tmp/treeman-*-linux-amd64/treemand ~/.local/bin/
    ```

=== "Composer"

    ```sh
    composer require --dev stubbedev/treeman
    vendor/bin/treeman --version
    ```

    The binaries are fetched lazily on first run; see
    [Composer install](composer.md).

=== "From source"

    ```sh
    git clone https://github.com/stubbedev/treeman
    cd treeman
    just install        # → $GOBIN/treeman + $GOBIN/treemand
    ```

    Needs the Go version from `go.mod`; `nix develop` provides it.

Shell completion is optional but recommended:

```sh
source <(treeman completion zsh)     # or bash; fish: treeman completion fish | source
```

## Set up a repository

```sh
cd ~/code/my-app
treeman init               # writes a .treeman.yaml tailored to the repo
treeman daemon install     # systemd --user / launchd unit, started now and at login
treeman doctor             # checks daemon, config, schema and registry drift
```

`treeman init` detects the package manager and migration framework and
writes a `.treeman.yaml` with a `databases:` block, the migrate command
and its inputs, and the patches that point your env files at the
per-worktree databases. Review it, then commit it. The
[configuration guide](configuration.md) explains every block.

## The daily loop

```sh
treeman worktree create PROJ-123
#   git worktree add .worktrees/PROJ-123 -b PROJ-123 origin/HEAD
#   copies .env, links node_modules, patches env files to the new DB names
#   daemon: create hooks → prepare (ensure → dump → migrate → snapshot → test clones)

cd "$(treeman worktree go PROJ-123)"
treeman worktree show PROJ-123     # databases, recent events, hook runs
treeman worktree logs              # event log for the current worktree

treeman worktree delete PROJ-123   # delete hooks → drop databases → git worktree remove
cd "$(treeman worktree back --remove)"   # back to the main checkout, removing this worktree if clean
```

`worktree create` returns as soon as the work is dispatched. Add
`--foreground` to stream progress and block until it is ready, or run
`treeman worktree wait PROJ-123` later. `treeman status` and
`treeman tui` show every worktree's health at a glance.

When migrations, dumps or lockfiles change, the daemon notices and
re-prepares the affected databases. Pulling a branch that only added a
migration applies it as a delta instead of rebuilding.

## Next steps

- [Concepts](concepts.md): slugs, the prepare pipeline, the daemon.
- [Configuration](configuration.md): databases, patches, hooks, ports.
- [Git workflow](git-workflow.md): pickers and `cd` helpers.
- [AI integration](mcp.md): drive treeman from Claude Code or Cursor.
