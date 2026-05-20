# treeman

**Per-worktree DB orchestrator with file watcher.** Spin up scoped
test databases per git worktree, tear them down on delete, keep them
in sync as migrations change.

Pure wire-protocol DB access (Go `database/sql` for MySQL +
PostgreSQL, the official mongo / redis / Elasticsearch SDKs); no
shelling out to `mysql` / `psql` / `mongosh` / `redis-cli` /
`docker exec`. Single global daemon, thin CLI client, SQLite-backed
event log.

> **v1.0+ is a complete Go rewrite** of the previous Rust workspace.
> See `pre-go-rewrite` tag for the last Rust commit. Drop-in: same
> YAML schema, same SQLite schema, same JSON-line RPC socket
> protocol — existing `~/.local/share/treeman/treeman.db` and
> `.treeman.yaml` files keep working.

---

## Why

The Rust implementation hit recurring tooling friction (cargo + nix +
clang + mold + sccache, slow `hm switch` cycle on every bug fix) and
two protocol bugs that didn't show up until kontainer-scale usage
(MySQL prepared-statement 1295 on `USE`/`SHOW CREATE`, watcher
dispatch handing the worktree path instead of the main repo root to
prepare). The Go port fixes both by construction: `database/sql`
defaults to MySQL's text protocol, and `prepare.Run` takes
`mainRepoRoot` + `worktreePath` as distinct arguments.

---

## Install

```sh
# From source — Go 1.23+ required.
git clone https://github.com/stubbedev/treeman
cd treeman
just install   # → $GOBIN/treeman + $GOBIN/treemand

# Or via nix.
nix profile install github:stubbedev/treeman
```

Pre-built binaries for `{linux,darwin}` × `{amd64,arm64}` ship with
every tagged release. See
[releases](https://github.com/stubbedev/treeman/releases).

---

## Quick start

```sh
# 1. Bootstrap config in your repo.
cd ~/code/my-laravel-app
treeman init               # writes .treeman.yaml

# 2. Install + start the daemon.
treeman daemon install     # systemd --user

# 3. Create a worktree end-to-end.
treeman wt create PROJ-123
#   ↳ git worktree add ../my-laravel-app-worktrees/PROJ-123 -b PROJ-123 main
#   ↳ symlinks .env, .env.testing, justfile, etc.
#   ↳ patches .env.testing's DB_DATABASE → my_app_testing_proj_123
#   ↳ runs postcreate hooks (composer install + yarn install)
#   ↳ prepare: ensure_db → load dump → migrate → snapshot → N paratest clones

# 4. Work in the worktree. Done with the ticket:
treeman wt delete PROJ-123
#   ↳ runs predelete hook (DB drops, FLUSHDB, ES index delete)
#   ↳ git worktree remove
```

---

## CLI reference

| Command | What |
|---|---|
| `treeman init` | Generate a starter `.treeman.yaml` |
| `treeman daemon {start,stop,restart,status,install,uninstall}` | Daemon lifecycle |
| `treeman wt {create,delete,list,register,unregister,finalize}` | Worktree lifecycle |
| `treeman prepare` | ensure → dump → migrate → snapshot → replicate |
| `treeman hook run <phase>` | Run a configured hook phase manually |
| `treeman logs {tail,grep}` | Query the SQLite event log |
| `treeman slug [path]` | Print the slug derived from a worktree path |
| `treeman config {validate,show [--resolved]}` | Config helpers |
| `treeman schema {dump,install}` | JSON Schema for `.treeman.yaml` (pending v1.1) |
| `treeman fw detect` | List detected migration + test frameworks |

---

## Configuration

Layered: global `~/.config/treeman/config.yaml` → per-repo
`.treeman.yaml` → per-repo `.treeman.local.yaml` (gitignored). Later
layers override.

See `.treeman.yaml` in any treeman-enabled repo for examples; the
top of the file carries a `# yaml-language-server: $schema=…`
modeline so editors with the YAML LSP get autocomplete.

### Hook groups (v1.0)

Each entry under `postcreate` / `predelete` / `postdelete` is a
**group** that spawns one detached driver via `setsid`. Within a
group, commands chain with `&&` (sequence). Across groups, drivers
run in **parallel**. The whole runner returns once drivers are
spawned — interactive `gwt`-style flows return in <2s regardless of
how slow the hooks themselves are.

```yaml
hooks:
  postcreate:
    # Three independent groups, all fire in parallel.
    - "composer install --no-interaction"
    - "yarn install --frozen-lockfile"
    - { run: "yarn install", cwd: frontend }

    # Group of two — sequence within. Runs in parallel with the above.
    - - "npm install"
      - "npm run build"
```

`precreate` is the one synchronous phase (each step awaited in
order, first non-zero exit aborts the create).

---

## License

Dual-licensed under Apache-2.0 OR MIT.
