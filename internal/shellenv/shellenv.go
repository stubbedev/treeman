// Package shellenv assembles the environment for treeman's user-facing
// subprocesses — lifecycle hooks, prepare phases, and migration/seed
// commands.
//
// The contract: a command declared in `.treeman.yaml` must resolve and
// behave exactly as it would if the user pasted it into their own
// terminal. They should never have to write it differently because
// treeman ran it.
//
// To honour that, baseEnv layers three sources:
//
//  1. The daemon's own os.Environ() — the floor. HOME, USER, LANG,
//     XDG_*, the OS essentials every process its user owns receives.
//     Tools like composer refuse to run without HOME.
//  2. The user's CLI-captured env (inheritedEnv, ultimately os.Environ()
//     at `treeman worktree create` time): their PATH, version-manager shims
//     (asdf, nvm, mise, rbenv, …), session overrides. Wins over floor.
//  3. The login-shell PATH (LoginShellPATH) is always merged into PATH.
//     Under `systemd --user` both the floor and an externally-created
//     worktree's empty inheritedEnv can miss the user's profile bin
//     dirs (e.g. ~/.nix-profile/bin), so a hook like `composer install`
//     fails with `env: 'php': No such file or directory`. The user's
//     real login shell always has those dirs; merging them in closes
//     the gap for every code path. With an inherited PATH the login
//     dirs are appended (the user's live session wins); without one
//     they LEAD the daemon floor, so a stale /usr/local/bin tool can't
//     shadow the profile bin the user's terminal would resolve (#121).
//
// CommandEnv then applies the directory's direnv environment on top,
// exactly as the user's direnv shell hook would on `cd`: a repo whose
// tools come from `.envrc` (`use flake`, `layout php`, …) resolves them
// in hooks and engine steps without any per-command wrapping (#182).
package shellenv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/stubbedev/treeman/internal/detach"
)

// CommandEnv returns the env a user-declared command run in dir gets:
// baseEnv, with dir's direnv environment applied when direnv is on the
// resulting PATH. Callers overlay their own scoping vars (TREEMAN_*) on
// top. A direnv that fails to load (a blocked or broken `.envrc`) is an
// error rather than a silently missing toolchain.
func CommandEnv(ctx context.Context, inheritedEnv map[string]string, dir string) (map[string]string, error) {
	env := baseEnv(inheritedEnv)
	if err := applyDirenv(ctx, env, dir); err != nil {
		return nil, err
	}
	return env, nil
}

// Flatten renders env as the KEY=VALUE list exec.Cmd.Env takes.
func Flatten(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

const direnvTimeout = 10 * time.Minute

// applyDirenv runs `direnv export json` in dir against env and applies
// the resulting diff to env in place: a string sets the variable, null
// unsets it. direnv computes the diff from env itself, so an inherited
// environment from another direnv directory is reverted first, the same
// as `cd` between two such directories in the user's terminal.
func applyDirenv(ctx context.Context, env map[string]string, dir string) error {
	bin := lookPathIn("direnv", env["PATH"])
	if bin == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, direnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "export", "json")
	cmd.Dir = dir
	cmd.Env = Flatten(env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("load direnv environment for %s (%s export json): %w: %s",
			dir, bin, err, strings.TrimSpace(stderr.String()))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	var diff map[string]*string
	if err := json.Unmarshal(out, &diff); err != nil {
		return fmt.Errorf("parse %s export json output for %s: %w", bin, dir, err)
	}
	for k, v := range diff {
		if v == nil {
			delete(env, k)
			continue
		}
		env[k] = *v
	}
	return nil
}

// lookPathIn resolves name against pathList the way the shell would,
// returning "" when no directory holds an executable of that name.
// exec.LookPath cannot be used: it searches the daemon's own PATH, not
// the one the command will run with.
func lookPathIn(name, pathList string) string {
	for dir := range strings.SplitSeq(pathList, string(os.PathListSeparator)) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// direnvStatePrefix marks the variables direnv keeps about what it has
// loaded (DIRENV_DIFF, DIRENV_DIR, …). The daemon's own are dropped from
// the floor: they describe whatever shell started the daemon, and direnv
// would revert that foreign diff over the user's PATH. The user's, from
// inheritedEnv, are kept so a worktree's `.envrc` replaces the one their
// terminal had loaded, as `cd` would.
const direnvStatePrefix = "DIRENV_"

// baseEnv returns the foundational env map: daemon floor, overlaid with
// the user's inheritedEnv, with the login-shell PATH always merged into
// PATH.
func baseEnv(inheritedEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(inheritedEnv)+32)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(k, direnvStatePrefix) {
			continue
		}
		merged[k] = v
	}
	daemonPATH := merged["PATH"]
	maps.Copy(merged, inheritedEnv)
	if p := inheritedEnv["PATH"]; p != "" {
		merged["PATH"] = MergePaths(p, LoginShellPATH())
	} else {
		merged["PATH"] = MergePaths(LoginShellPATH(), daemonPATH)
	}
	return merged
}

// LoginShellPATH returns the PATH a fresh login shell of the user
// running treemand would have. Best-effort — returns "" on failure, in
// which case MergePaths leaves the existing PATH untouched.
//
// Only a successful probe is cached: the daemon's owning user and their
// shell config don't change over its lifetime, and spawning a shell per
// call is wasteful. A failed probe is NOT cached (#121) — at daemon
// start under systemd the profile (nix, …) may not be ready yet, and
// caching that "" starved every hook of the user's bin dirs until the
// daemon restarted. Failures are retried at most once per
// loginProbeRetry so a permanently broken rc file can't add a shell
// spawn (up to the probe timeout) to every hook.
var LoginShellPATH = cachedLoginShellPATH

// loginProbe is the uncached probe; a var so tests can stub it.
var loginProbe = probeLoginShellPATH

const loginProbeRetry = 30 * time.Second

var loginPATH struct {
	mu         sync.Mutex
	value      string
	lastFailed time.Time
}

func cachedLoginShellPATH() string {
	loginPATH.mu.Lock()
	defer loginPATH.mu.Unlock()
	if loginPATH.value != "" {
		return loginPATH.value
	}
	if !loginPATH.lastFailed.IsZero() && time.Since(loginPATH.lastFailed) < loginProbeRetry {
		return ""
	}
	p := loginProbe()
	if p == "" {
		loginPATH.lastFailed = time.Now()
		return ""
	}
	loginPATH.value = p
	return p
}

func probeLoginShellPATH() string {
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
	var lastErr error
	for _, flags := range [][]string{{"-ilc"}, {"-lc"}, {"-ic"}, {"-c"}} {
		args := append(append([]string{}, flags...), pathProbeCommand)
		probe := exec.CommandContext(ctx, shell, args...)
		if err := detach.NewSession(probe); err != nil {
			lastErr = err
			break
		}
		out, err := probe.Output()
		if err != nil {
			lastErr = err
			continue
		}
		p, err := parseProbedPATH(out)
		if err != nil {
			lastErr = err
			slog.Warn("shellenv: discarding login-shell PATH probe output",
				"shell", shell, "flags", flags, "err", err, "raw", string(out))
			continue
		}
		return p
	}
	slog.Warn("shellenv: login-shell PATH probe failed; hooks run without the user's profile PATH (will retry)",
		"shell", shell, "err", lastErr)
	return ""
}

const (
	pathProbeBegin = "__TREEMAN_PATH_BEGIN__"
	pathProbeEnd   = "__TREEMAN_PATH_END__"
)

const pathProbeCommand = `printf '\n` + pathProbeBegin + `%s` + pathProbeEnd + `\n' "$PATH"`

func parseProbedPATH(out []byte) (string, error) {
	_, framed, found := strings.CutLast(string(out), pathProbeBegin)
	if !found {
		return "", errors.New("probe output has no PATH begin sentinel")
	}
	p, _, found := strings.Cut(framed, pathProbeEnd)
	if !found {
		return "", errors.New("probe output has no PATH end sentinel")
	}
	if p == "" {
		return "", errors.New("probed PATH is empty")
	}
	for dir := range strings.SplitSeq(p, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("probed PATH entry %q is not absolute", dir)
		}
		if strings.ContainsFunc(dir, unicode.IsControl) {
			return "", fmt.Errorf("probed PATH entry %q contains control characters", dir)
		}
	}
	return p, nil
}

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
