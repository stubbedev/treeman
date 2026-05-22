# treeman zsh shim — `tm` shell wrapper around `treeman wt`.
#
# `treeman wt switch` + `treeman wt back` print resolved paths on
# stdout; this function wraps them so `tm foo` and `tm -` change
# directory in the parent shell.
#
# Install: source this file from your .zshrc (or symlink into
# ~/.config/zsh/functions and `autoload`):
#
#     source /path/to/treeman/contrib/tm.zsh
#
# Usage:
#     tm PROJ-1234         # cd to existing worktree, or report missing
#     tm PROJ-1234 -c      # create + cd to a new worktree
#     tm -                 # cd back to main repo
#     tm - --remove        # cd back + drop current wt (if clean)
#     tm list              # passthrough to `treeman wt list`
#     tm new FOO           # passthrough; useful when --create needs flags
#
# All flags after the target name are forwarded to the underlying
# `treeman wt switch`/`treeman wt back` invocation.

tm() {
  emulate -L zsh
  if (( $# == 0 )); then
    treeman wt list
    return $?
  fi

  case "$1" in
    -|back)
      shift
      local main
      if ! main=$(treeman wt back "$@"); then
        return $?
      fi
      [[ -n $main ]] && builtin cd -- "$main"
      return 0
      ;;
    list|ls)
      shift
      treeman wt list "$@"
      return $?
      ;;
    new|create)
      shift
      # `tm new BRANCH [...]` → forward to switch --create so the
      # cd-after-create UX is the same as the bare `tm BRANCH -c`
      # form.
      local branch="$1"
      shift || true
      local target
      if ! target=$(treeman wt switch "$branch" --create "$@"); then
        return $?
      fi
      [[ -n $target ]] && builtin cd -- "$target"
      return 0
      ;;
    -h|--help|help)
      cat <<'USAGE'
tm — treeman shim
  tm <name>           cd to existing worktree
  tm <name> -c        create + cd to new worktree
  tm new <name>       same as `tm <name> -c`
  tm - [--remove]     cd back to main repo (with --remove: drop current wt if clean)
  tm list             list active worktrees
USAGE
      return 0
      ;;
  esac

  local name="$1"
  shift
  # Translate the short `-c` flag to the long `--create` form the Go
  # CLI exposes; everything else passes through.
  local args=()
  local want_create=0
  while (( $# > 0 )); do
    case "$1" in
      -c|--create) want_create=1 ;;
      *) args+=("$1") ;;
    esac
    shift
  done
  if (( want_create )); then
    args+=(--create)
  fi

  local target
  if ! target=$(treeman wt switch "$name" "${args[@]}"); then
    return $?
  fi
  [[ -n $target ]] && builtin cd -- "$target"
}

# Completion: complete worktree names from `treeman wt list`.
# NO_COLOR=1 suppresses ANSI escapes so the SLUG column parses cleanly.
_tm_complete() {
  local -a names
  names=("${(@f)$(NO_COLOR=1 treeman wt list 2>/dev/null | awk 'NR>1 {print $2}')}")
  _describe 'worktree' names
}
compdef _tm_complete tm 2>/dev/null || true
