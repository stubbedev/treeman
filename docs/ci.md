# CI without a daemon

On a CI runner there is no user session, no daemon and usually no
linked worktree: the job has one checkout and wants its databases
prepared before the test suite starts. `treeman prepare --no-daemon`
does exactly that.

```sh
treeman prepare --no-daemon --json
```

It runs the normal prepare pipeline (ensure → dump → migrate →
snapshot → test clones) and the post-engine hooks synchronously in the
CLI process, then exits. It never connects to a daemon socket,
subscribes to events or starts a daemon, even if one is reachable or a
stale socket exists. A failed pipeline or a fatal hook exits non-zero;
`--json` prints the usual structured outcomes.

`--no-daemon` beats `--foreground`. Without it, bare `prepare` may use
daemon RPC, and `--foreground` streams live daemon progress and may
start a daemon. Don't substitute daemon-backed `worktree` lifecycle
commands in CI.

## What the job must provide

- **Engine services and credentials** for every configured database,
  exactly as on a developer machine (see
  [credential resolution](configuration.md#credential-resolution-from-env)).
- **An isolated state directory.** The registry and snapshot table live
  in SQLite at `TREEMAN_DB_PATH`. Point it at a writable per-build
  directory, together with `TMPDIR` and `XDG_DATA_HOME`, so concurrent
  builds never share a database file (including its WAL/SHM sidecars).
- **A job-unique slug** when two jobs can prepare the same branch on one
  machine or against one database server. Set `TREEMAN_SLUG_SUFFIX` to
  something unique per job (`$GITHUB_RUN_ID`, a job id, …) so the
  `main_<branch>_<tag>` slug, and with it every rendered database name,
  stays disjoint.
- **Writable engine storage** with room for the template snapshots and
  clones. Snapshots live in the database engines, not on disk next to
  SQLite.

Isolating SQLite alone does not isolate shared database namespaces:
concurrent builds also need distinct database names (the slug suffix
above) or isolated engine instances. Clean up engine resources and the
per-build state directory when the job ends; the daemon's periodic
snapshot and log retention sweeps do not run in this mode.

## Selecting a config

`--config /absolute/path/config.yaml` (or `TREEMAN_CONFIG`; the flag
wins) replaces the default **global** config layer. Unlike the default
global file, an explicit file may also carry repo-scoped settings
(`databases`, `patches`, `hooks`, …), so a repository can commit its CI
config and pass it with `--config` instead of shipping a
`.treeman.yaml`. A repository `.treeman.yaml` still loads afterwards and
wins for repo-scoped settings. Global-only settings (`snapshots`,
`daemon`, `logs`) belong in the explicit file.

This selects configuration for in-process execution only; it does not
reconfigure an already running daemon.

## Example: GitHub Actions with Composer

```sh
composer install --no-interaction
export TREEMAN_COMPOSER_CACHE="$HOME/.cache/treeman/composer"
vendor/bin/treeman --version          # warms the binary cache
export TREEMAN_COMPOSER_OFFLINE=1

TREEMAN_BUILD_STATE=$(mktemp -d "$PWD/.treeman-ci.XXXXXXXX")
export TREEMAN_DB_PATH="$TREEMAN_BUILD_STATE/treeman.db"
export XDG_DATA_HOME="$TREEMAN_BUILD_STATE/data"
export TMPDIR="$TREEMAN_BUILD_STATE/tmp"
mkdir -p "$TMPDIR" "$XDG_DATA_HOME"

cat > "$TREEMAN_BUILD_STATE/config.yaml" <<'YAML'
snapshots:
  cap_per_repo: 8
YAML
export TREEMAN_CONFIG="$TREEMAN_BUILD_STATE/config.yaml"
export TREEMAN_SLUG_SUFFIX="${GITHUB_RUN_ID:-$(date +%s)}"

vendor/bin/treeman prepare --no-daemon --repo "$PWD" --worktree "$PWD"
vendor/bin/phpunit
```

See [Composer install](composer.md) for the binary cache and offline
mode.

## Gating on the daemon

Where a daemon *is* expected (a self-hosted runner with a long-lived
user session), `treeman daemon status --fail-if-down` exits `3` when it
is unreachable, distinct from `1` for a broken command. `treeman
worktree wait <name>` blocks until a worktree's finalize completes and
exits non-zero if it failed. See [exit codes](cli.md#exit-codes).
