// Package shellenv assembles the environment for treeman's user-facing
// subprocesses — lifecycle hooks, prepare phases, and migration/seed
// commands.
//
// The contract: a command declared in `.treeman.yaml` must resolve and
// behave exactly as it would if the user pasted it into their own
// terminal. They should never have to write it differently because
// treeman ran it.
//
// To honour that, BaseEnv layers three sources:
//
//  1. The daemon's own os.Environ() — the floor. HOME, USER, LANG,
//     XDG_*, the OS essentials every process its user owns receives.
//     Tools like composer refuse to run without HOME.
//  2. The user's CLI-captured env (inheritedEnv, ultimately os.Environ()
//     at `treeman wt create` time): their PATH, version-manager shims
//     (asdf, nvm, mise, rbenv, …), session overrides. Wins over floor.
//  3. The login-shell PATH (LoginShellPATH) is always merged into PATH.
//     Under `systemd --user` both the floor and an externally-created
//     worktree's empty inheritedEnv can miss the user's profile bin
//     dirs (e.g. ~/.nix-profile/bin), so a hook like `composer install`
//     fails with `env: 'php': No such file or directory`. The user's
//     real login shell always has those dirs; merging them in closes
//     the gap for every code path.
package shellenv

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"time"
)

// BaseEnv returns the foundational env map: daemon floor, overlaid with
// the user's inheritedEnv, with the login-shell PATH always merged into
// PATH. Callers overlay their own scoping vars (TREEMAN_*) on top.
func BaseEnv(inheritedEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(inheritedEnv)+32)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			merged[k] = v
		}
	}
	maps.Copy(merged, inheritedEnv)
	merged["PATH"] = MergePaths(merged["PATH"], LoginShellPATH())
	return merged
}

// LoginShellPATH returns the PATH a fresh login shell of the user
// running treemand would have. The result is cached: the daemon's
// owning user and their shell config don't change over its lifetime,
// and spawning a shell per call is wasteful. Best-effort — returns ""
// on any failure, in which case MergePaths leaves the existing PATH
// untouched.
var LoginShellPATH = sync.OnceValue(func() string {
	// Bound the shell probes — an interactive shell with a broken rc
	// file could otherwise hang the daemon.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	shell := userLoginShell(ctx)
	if shell == "" {
		return ""
	}
	// We want the exact PATH the user sees when they type a command in
	// their own terminal. Where PATH gets set differs by shell and rc
	// file: zsh interactive shells read ~/.zshrc, login shells also read
	// ~/.zprofile / ~/.zlogin; bash login reads ~/.bash_profile,
	// interactive reads ~/.bashrc; POSIX sh reads $ENV when interactive.
	// To capture the superset, probe an interactive login shell first
	// (`-ilc`), then fall back to flag sets that pickier shells accept.
	// `printf %s "$PATH"` keeps stdout clean of any rc-file noise.
	for _, flags := range [][]string{{"-ilc"}, {"-lc"}, {"-ic"}, {"-c"}} {
		args := append(append([]string{}, flags...), `printf %s "$PATH"`)
		out, err := exec.CommandContext(ctx, shell, args...).Output()
		if err != nil {
			continue
		}
		if p := strings.TrimSpace(string(out)); p != "" {
			return p
		}
	}
	slog.Debug("shellenv: login-shell PATH probe failed", "shell", shell)
	return ""
})

// userLoginShell resolves the login shell of the user running this
// process. It reads /etc/passwd via `getent` (authoritative even under
// systemd, where $SHELL is often unset), falling back to $SHELL and
// then /bin/sh.
func userLoginShell(ctx context.Context) string {
	if u, err := user.Current(); err == nil && u.Uid != "" {
		if out, err := exec.CommandContext(ctx, "getent", "passwd", u.Uid).Output(); err == nil {
			// passwd line: name:passwd:uid:gid:gecos:home:shell
			fields := strings.Split(strings.TrimSpace(string(out)), ":")
			if len(fields) >= 7 && fields[6] != "" {
				return fields[6]
			}
		}
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// MergePaths appends any directories in extra (a PATH-list) not already
// present in base, preserving base's precedence. Either argument may be
// empty.
func MergePaths(base, extra string) string {
	if extra == "" {
		return base
	}
	if base == "" {
		return extra
	}
	sep := string(os.PathListSeparator)
	seen := make(map[string]struct{})
	for d := range strings.SplitSeq(base, sep) {
		if d != "" {
			seen[d] = struct{}{}
		}
	}
	var out strings.Builder
	out.WriteString(base)
	for d := range strings.SplitSeq(extra, sep) {
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out.WriteString(sep)
		out.WriteString(d)
	}
	return out.String()
}
