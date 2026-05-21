# CLI reference

[← back to README](../README.md)

| Command | What |
|---|---|
| `treeman init` | Generate a starter `.treeman.yaml` from cwd markers |
| `treeman doctor` | Health-check the local setup (daemon, config, registry drift) |
| `treeman daemon {start,stop,status,install,uninstall}` | Daemon lifecycle |
| `treeman wt {create,delete,list,register,unregister,finalize}` | Worktree lifecycle |
| `treeman wt show <name>` | Per-worktree dossier — state, recent events, hook runs |
| `treeman wt logs <name>` | Tail events scoped to one worktree |
| `treeman wt wait <name>` | Block until the daemon's finalize completes (CI sync primitive) |
| `treeman wt switch <name> [--create]` | Print worktree path (for shell `cd $(…)`) |
| `treeman wt back [--remove]` | Print main repo path; optionally drop clean worktree |
| `treeman prepare` | ensure → dump → migrate → snapshot → replicate |
| `treeman hook run <phase>` | Run a configured hook phase manually |
| `treeman logs {tail,grep,hooks}` | Query the SQLite event log (see flags below) |
| `treeman slug [path]` | Print the slug derived from a worktree path |
| `treeman config {validate,show [--resolved]}` | Config helpers |
| `treeman schema {dump,install}` | JSON Schema for `.treeman.yaml` |
| `treeman fw detect` | List detected migration + test frameworks |
| `treeman completion {bash,zsh,fish,pwsh}` | Print shell completion script |
| `treeman mcp [--allow-mutations] [--allow-shell]` | Run the MCP server (stdio) — see [MCP / AI integration](mcp.md) |

Destructive subcommands (`wt delete`, `daemon uninstall`) prompt
on a TTY; pass `--yes`/`-y` to skip the prompt, or run in a
non-interactive shell (scripts, CI, `&!`). Subcommand typos
print `did you mean: …` with the closest matches by Levenshtein
distance.

`treeman <cmd> --help` for full flag listings.

## Log filters

`logs tail` / `logs grep` share a filter surface:

```sh
treeman logs tail --follow                      # stream new events
treeman logs tail --worktree PROJ-1234          # scope to one worktree
treeman logs tail --level warn --level error    # repeatable
treeman logs tail --since 5m                    # duration or RFC3339 timestamp
treeman logs tail --event-type wt_finalize_done
treeman logs tail --json | jq .                 # machine-readable
treeman logs grep "snapshot cache" --regex
treeman logs grep checksum --search-payload     # match payload_json instead
treeman logs hooks PROJ-1234                    # last N hook_runs for a worktree
```

## Shell completion

Source the completion script from your shell rc:

```sh
# bash (~/.bashrc)
source <(treeman completion bash)

# zsh (~/.zshrc)
source <(treeman completion zsh)

# fish (~/.config/fish/completions/treeman.fish)
treeman completion fish > ~/.config/fish/completions/treeman.fish
```

## Output, color, paging

treeman prints colored, symbol-prefixed status lines to a TTY and
degrades to plain ASCII when stdout is piped, redirected, or
`NO_COLOR=1` is set. `FORCE_COLOR=1` / `CLICOLOR_FORCE=1` force
colors on even when piping (useful for `treeman ... | less -R`).

Read commands that may produce more than a screen of output
(`treeman logs tail|grep`, `treeman wt show`, `treeman config show`)
auto-page through `$PAGER` (default: `less -FRX` — `-F` quits if the
output fits on one screen, `-R` keeps colors, `-X` skips the
alt-screen). Disable per-invocation with `--no-pager`, or globally
with `TREEMAN_NO_PAGER=1` / `PAGER=`. `--follow` and `--json` always
bypass the pager.

`--json` is supported on `treeman daemon status`, `treeman wt list`,
`treeman slug`, `treeman fw detect`, `treeman logs {tail,grep,hooks}`,
and `treeman doctor` — emits one object (or one per row) suitable
for `jq` consumption.

## Environment variables

| Variable | Effect | Default |
|---|---|---|
| `NO_COLOR` | Disable ANSI color when non-empty. | — |
| `FORCE_COLOR` / `CLICOLOR_FORCE` | Force ANSI color even when stdout is piped. | — |
| `TERM=dumb` | Disable ANSI color regardless of TTY detection. | — |
| `LANG` / `LC_ALL` / `LC_CTYPE` | Non-UTF-8 locale falls back to ASCII symbols (`[ok]`, `[x]`, `->`). | host locale |
| `PAGER` | Pager binary for long output. Set empty to disable. | `less -FRX` |
| `TREEMAN_NO_PAGER=1` | Globally disable paging. | — |
| `XDG_DATA_HOME` | Root for the SQLite event log (`<XDG_DATA_HOME>/treeman/treeman.db`). | `~/.local/share` |
| `XDG_RUNTIME_DIR` | Root for the daemon's unix socket (`<XDG_RUNTIME_DIR>/treeman.sock`). | `~/.cache` fallback |

All variables are read at process start; restart the daemon
(`treeman daemon stop && treeman daemon start`) after changing
`XDG_*` to relocate state.
