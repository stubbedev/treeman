# Git workflow

`treeman git` bundles the daily git verbs with a built-in interactive
picker (no fzf needed), worktree-aware branch routing and shell
completion. All UI renders to stderr, so stdout stays clean for
`cd "$(…)"` wrappers.

```sh
treeman git commit            # message prompt, ticket prefix taken from the branch
treeman git commit fix login  # same, prompt pre-filled (commits directly when scripted)
treeman git push              # guards protected branches and upstream divergence
treeman git add               # multi-select stage: M → add -p, D/A → add
treeman git switch            # picker: live worktrees first (* dirty, ! unpushed), then branches
treeman git switch KON-1234   # checkout-or-create, worktree-aware; prints the destination path
treeman git log               # interactive log: show / cherry-pick / revert / copy hash
treeman git diff --pick       # three-dot diff against a picked branch (--patch writes a .diff)
treeman git stash pop         # pick a stash
treeman git amend|undo|discard|branch-delete|sync-branch|fixup
```

Every subcommand and flag is listed in the
[CLI reference](cli.md#treeman-git).

## Picker keys

| Key | Action |
|---|---|
| typing | filter |
| Enter | accept |
| Tab | mark (multi-select pickers) |
| Ctrl+C | cancel the step (in `switch`, opens the branch wizard) |
| Esc | quit the whole command with no action |

`TREEMAN_PICKER_HEIGHT` sets the list height (default 10). Colours
respect `NO_COLOR` and `--color`.

## Changing directory

A child process can't `cd` its parent shell, so commands that land you
somewhere print the destination path and a two-line shell function does
the `cd`:

```sh
gwt() { local p; p=$(treeman worktree switch "$@") && [ -n "$p" ] && cd "$p"; }
gcb() { local p; p=$(treeman git switch      "$@") && [ -n "$p" ] && cd "$p"; }
```

`gwt` always lands in a worktree (creating one if needed); `gcb` may
check the branch out in place. For a ready-made `tm` function around
`worktree go` / `worktree back`, see
[shell integration](cli.md#shell-integration).
