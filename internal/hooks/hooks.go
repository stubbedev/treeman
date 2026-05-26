// Package hooks runs the configured hook entries for a worktree
// phase.
//
// Model: every entry under setup / teardown is a
// "group". Each group spawns one detached driver via setsid; the
// driver runs the group's commands chained with `&&` so a failure
// short-circuits the rest of the group. Groups never wait for each
// other — they all kick off in parallel and `RunHooks` returns as
// soon as the drivers are spawned.
//
// `setup` is the one synchronous phase via RunHooks.
package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
)

// RunOutcome bundles every group's status.
type RunOutcome struct {
	Groups            []GroupOutcome
	AggregateExitCode int // populated by RunHooks; always 0 for the async runner
}

// GroupOutcome — one row per group.
type GroupOutcome struct {
	// Command is the rendered shell string the driver ran
	// (sequence of `( cd <cwd> && <cmd> ) && …`).
	Command string
	// PID of the detached driver, or 0 when sync.
	PID int
	// LogPath under `<worktree>/.treeman-hooks/<phase>-<n>.log`,
	// empty for sync phases.
	LogPath string
	// ExitCode is populated by the sync phase runner.
	ExitCode   int
	StdoutTail string
	StderrTail string
	// LogBody is the full captured merged stdout+stderr for the group,
	// kept verbatim (ANSI escapes preserved). Populated by the
	// wait=true path so `PersistOutcome` can stream it into the
	// hook_log_chunks table.
	LogBody []byte
}

// RunHooksOrphan is the lifecycle-watcher variant of RunHooks. The
// working tree at `worktreePath` no longer exists (a `git worktree
// remove` outside the treeman CLI already deleted it), so:
//
//   - log files cannot live under the working tree — they are written
//     to `logDir` instead (the daemon owns a per-repo path under
//     XDG_STATE_HOME for this).
//   - drivers cwd into `repoRoot` instead of `worktreePath`. User
//     scripts that want the deleted-tree path read it from the
//     `TREEMAN_WORKTREE` env var.
//
// Otherwise identical to RunHooks: each group spawns one detached
// setsid driver; when `wait` is true the call blocks until they all
// exit.
func RunHooksOrphan(
	ctx context.Context,
	phase string,
	entries []config.Action,
	repoRoot, worktreePath, slug, logDir string,
	inheritedEnv map[string]string,
	wait bool,
) (RunOutcome, error) {
	out := RunOutcome{}
	if len(entries) == 0 {
		return out, nil
	}
	if logDir == "" {
		return out, fmt.Errorf("RunHooksOrphan: empty logDir")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return out, fmt.Errorf("create %s: %w", logDir, err)
	}
	out.Groups = make([]GroupOutcome, 0, len(entries))
	cmds := make([]*exec.Cmd, 0, len(entries))
	for i, entry := range entries {
		if len(entry.Run) == 0 {
			continue
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", phase, i))
		// Default cwd is the repo root, not the (deleted) worktree.
		cmdStr, err := renderAction(entry, repoRoot)
		if err != nil {
			return out, err
		}
		c, err := spawnDetached(cmdStr, repoRoot, worktreePath, repoRoot, slug, logPath, inheritedEnv, wait)
		if err != nil {
			return out, err
		}
		cmds = append(cmds, c)
		out.Groups = append(out.Groups, GroupOutcome{
			Command: cmdStr,
			PID:     c.Process.Pid,
			LogPath: logPath,
		})
	}
	if wait {
		for idx, c := range cmds {
			if err := c.Wait(); err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					out.Groups[idx].ExitCode = exitErr.ExitCode()
				} else {
					out.Groups[idx].ExitCode = -1
				}
			}
		}
	}
	return out, nil
}

// RunHooks spawns one detached setsid driver per group, in parallel.
// Logs go to `<worktree>/.treeman-hooks/<phase>-<group-idx>.log`.
//
// When `wait` is false (legacy callers, tests, fire-and-forget
// dispatch), returns as soon as every group is spawned — groups
// continue running under the new session and are reaped by a
// detached goroutine. When `wait` is true, blocks until every
// group exits, so the caller can rely on the phase being complete
// before invoking work that depends on it (e.g. `prepare.Run`
// reading vendor/ that a setup `composer install` populated).
// Groups still run in parallel under either flag; `wait` only
// changes whether RunHooks itself blocks on their completion.
func RunHooks(
	ctx context.Context,
	phase string,
	entries []config.Action,
	repoRoot, worktreePath, slug string,
	inheritedEnv map[string]string,
	wait bool,
) (RunOutcome, error) {
	out := RunOutcome{}
	if len(entries) == 0 {
		return out, nil
	}
	logDir := filepath.Join(worktreePath, ".treeman-hooks")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return out, fmt.Errorf("create %s: %w", logDir, err)
	}
	out.Groups = make([]GroupOutcome, 0, len(entries))
	cmds := make([]*exec.Cmd, 0, len(entries))
	for i, entry := range entries {
		if len(entry.Run) == 0 {
			continue
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", phase, i))
		cmdStr, err := renderAction(entry, worktreePath)
		if err != nil {
			return out, err
		}
		c, err := spawnDetached(cmdStr, worktreePath, worktreePath, repoRoot, slug, logPath, inheritedEnv, wait)
		if err != nil {
			return out, err
		}
		cmds = append(cmds, c)
		out.Groups = append(out.Groups, GroupOutcome{
			Command: cmdStr,
			PID:     c.Process.Pid,
			LogPath: logPath,
		})
	}
	if wait {
		for idx, c := range cmds {
			if err := c.Wait(); err != nil {
				// Don't abort the whole phase — surface the
				// exit code on the outcome and let the caller
				// decide. The same group's stdout/stderr are
				// already in its log file.
				if exitErr, ok := err.(*exec.ExitError); ok {
					out.Groups[idx].ExitCode = exitErr.ExitCode()
				} else {
					out.Groups[idx].ExitCode = -1
				}
			}
			// Slurp the full merged stdout+stderr log into the
			// outcome so PersistOutcome can persist it as a
			// hook_log_chunks row. The on-disk file stays put so
			// `tail -f` workflows keep working; the DB copy is what
			// survives worktree teardown.
			if out.Groups[idx].LogPath != "" {
				out.Groups[idx].LogBody = readFileBytes(out.Groups[idx].LogPath)
				if out.Groups[idx].ExitCode != 0 {
					out.Groups[idx].StderrTail = lastBytes(out.Groups[idx].LogBody, 4096)
				}
			}
		}
	}
	return out, nil
}

// readFileBytes returns the full body of path, or nil on any I/O
// error. Used to copy the on-disk hook log into the DB chunk row.
func readFileBytes(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// lastBytes returns the trailing n bytes of b as a string. Used to
// stash a short tail in stderr_tail for the `treeman logs hooks`
// table-rendered listing.
func lastBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// renderAction turns one Action into a single shell string. Steps
// chain with ` && ` so a failure short-circuits the rest. The
// action's cwd applies group-wide:
//
//   - Host execution:  `( cd <cwd> && <step1> && <step2> )`
//   - Container exec:  `<engine> exec [-w cwd] <id> sh -c '<step1>'
//     && <engine> exec [-w cwd] <id> sh -c '<step2>'`
//
// `defaultCwd` is the worktree root — used when the action's `cwd`
// is empty on the host path. In the container path the absence of
// `cwd` means "use the container's WORKDIR" (no `-w` flag).
func renderAction(a config.Action, defaultCwd string) (string, error) {
	if len(a.Run) == 0 {
		return "", nil
	}
	if a.Container == "" && a.ComposeService == "" {
		cwd := a.Cwd
		if cwd == "" {
			cwd = defaultCwd
		}
		parts := make([]string, 0, len(a.Run))
		for _, step := range a.Run {
			parts = append(parts, "( cd "+shellSingleQuote(cwd)+" && "+step+" )")
		}
		return strings.Join(parts, " && "), nil
	}
	// Container exec path.
	engine := a.Engine
	if engine == "" {
		engine = "docker"
	}
	id, err := containerip.ContainerID(containerip.Opts{
		Container:      a.Container,
		ComposeService: a.ComposeService,
		ComposeProject: a.ComposeProject,
		Engine:         engine,
	})
	if err != nil {
		return "", fmt.Errorf("action container resolve: %w", err)
	}
	parts := make([]string, 0, len(a.Run))
	for _, step := range a.Run {
		args := []string{"exec"}
		if a.Cwd != "" {
			args = append(args, "-w", a.Cwd)
		}
		args = append(args, id, "sh", "-c", step)
		parts = append(parts, shellJoin(append([]string{engine}, args...)))
	}
	return strings.Join(parts, " && "), nil
}

func shellJoin(argv []string) string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shellSingleQuote(a))
	}
	return strings.Join(out, " ")
}

// shellSingleQuote wraps a string in single quotes, escaping any
// embedded ` ' ` as `'\”` per POSIX.
func shellSingleQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, c := range s {
		if c == '\'' {
			b.WriteString(`'\''`)
		} else {
			b.WriteRune(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// spawnDetached forks a `setsid /bin/sh -c <cmd>` child with
// stdout+stderr redirected to logPath. Env is cleared then layered
// with the caller's inheritedEnv plus the three standard overlay
// vars (TREEMAN_MAIN_ROOT, TREEMAN_WORKTREE, TREEMAN_SLUG). Returns
// the *exec.Cmd so the caller can either reap it inline (`Wait()`
// on the wait=true path) or fire-and-forget via a detached goroutine.
//
// `cwd` is the child's working directory. For the normal flow this
// equals `worktreePath`; for the lifecycle-orphan flow (worktree
// already deleted) it is the repo root, while `worktreePath` is the
// deleted absolute path — surfaced in the env so user scripts can
// still reference it.
func spawnDetached(cmdStr, cwd, worktreePath, repoRoot, slug, logPath string, inheritedEnv map[string]string, wait bool) (*exec.Cmd, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open hook log %s: %w", logPath, err)
	}
	defer logFile.Close()

	c := exec.Command("/bin/sh", "-c", cmdStr)
	c.Dir = cwd
	c.Env = buildEnv(inheritedEnv, repoRoot, worktreePath, slug)
	c.Stdout = logFile
	c.Stderr = logFile
	c.Stdin = nil
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("setsid spawn `%s`: %w", cmdStr, err)
	}
	if !wait {
		// Caller doesn't want to block on completion. Reap the child
		// asynchronously so zombies don't pile up against this
		// daemon process.
		go func() { _ = c.Wait() }()
	}
	return c, nil
}

// runForeground runs one step synchronously, captures tails, returns
// a GroupOutcome.
func runForeground(ctx context.Context, cmdStr, cwd, repoRoot, worktreePath, slug string, inheritedEnv map[string]string) (GroupOutcome, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
	c.Dir = cwd
	c.Env = buildEnv(inheritedEnv, repoRoot, worktreePath, slug)
	stdoutPipe, _ := c.StdoutPipe()
	stderrPipe, _ := c.StderrPipe()
	if err := c.Start(); err != nil {
		return GroupOutcome{Command: cmdStr, ExitCode: -1}, fmt.Errorf("spawn `%s`: %w", cmdStr, err)
	}
	stdoutTail := captureTail(stdoutPipe, 16*1024)
	stderrTail := captureTail(stderrPipe, 16*1024)
	err := c.Wait()
	g := GroupOutcome{Command: cmdStr, StdoutTail: stdoutTail, StderrTail: stderrTail}
	if exitErr, ok := err.(*exec.ExitError); ok {
		g.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		g.ExitCode = -1
	}
	return g, nil
}

func captureTail(r io.Reader, cap int) string {
	if r == nil {
		return ""
	}
	all, _ := io.ReadAll(r)
	if len(all) > cap {
		all = all[len(all)-cap:]
	}
	return string(all)
}

// buildEnv assembles the env shipped to a hook subprocess. The goal:
// user commands declared in `.treeman.yaml` should see the same env
// they would see if the user pasted them into their terminal.
//
// The model:
//
//  1. Start from the daemon's own `os.Environ()`. This is the floor:
//     HOME, USER, SHELL, LANG, XDG_*, PATH — every essential the OS
//     gives any process its user owns. Without this, commands like
//     `composer install` fail because they refuse to run when HOME is
//     unset, even though every interactive shell on the host has HOME.
//  2. Overlay the user's CLI-captured env (`inheritedEnv`, ultimately
//     os.Environ() at `treeman wt create` time). The user's PATH,
//     language version manager shims (asdf, nvm, mise, rbenv, …),
//     and any session-specific overrides take precedence over the
//     daemon's floor.
//  3. Overlay the treeman scoping vars (TREEMAN_MAIN_ROOT, _WORKTREE,
//     _SLUG) so user scripts can address them. These always win.
//
// Layering order means the lifecycle watcher's empty `inheritedEnv`
// is still safe (the daemon's env fills the floor), and a user with a
// rich shell setup still gets their PATH / shims propagated.
func buildEnv(inheritedEnv map[string]string, repoRoot, worktreePath, slug string) []string {
	merged := make(map[string]string, len(inheritedEnv)+32)
	for _, kv := range os.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				merged[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range inheritedEnv {
		merged[k] = v
	}
	merged["TREEMAN_MAIN_ROOT"] = repoRoot
	merged["TREEMAN_WORKTREE"] = worktreePath
	merged["TREEMAN_SLUG"] = slug
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	return out
}
