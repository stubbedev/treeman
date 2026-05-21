// Package hooks runs the configured hook entries for a worktree
// phase.
//
// Model: every entry under postcreate / predelete / postdelete is a
// "group". Each group spawns one detached driver via setsid; the
// driver runs the group's commands chained with `&&` so a failure
// short-circuits the rest of the group. Groups never wait for each
// other — they all kick off in parallel and `RunHooks` returns as
// soon as the drivers are spawned.
//
// `precreate` is the one synchronous phase via RunPrecreateHooks.
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
	AggregateExitCode int // populated by RunPrecreateHooks; always 0 for the async runner
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
// reading vendor/ that a postcreate `composer install` populated).
// Groups still run in parallel under either flag; `wait` only
// changes whether RunHooks itself blocks on their completion.
func RunHooks(
	ctx context.Context,
	phase string,
	entries []config.HookEntry,
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
		if len(entry.Steps) == 0 {
			continue
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", phase, i))
		cmdStr, err := renderGroupForEntry(entry, worktreePath)
		if err != nil {
			return out, err
		}
		c, err := spawnDetached(cmdStr, worktreePath, repoRoot, slug, logPath, inheritedEnv, wait)
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
		}
	}
	return out, nil
}

// RunPrecreateHooks is the synchronous variant. Each SingleStep
// awaited in order; first non-zero exit aborts.
func RunPrecreateHooks(
	ctx context.Context,
	steps []config.SingleStep,
	repoRoot, worktreePath, slug string,
	inheritedEnv map[string]string,
) (RunOutcome, error) {
	out := RunOutcome{Groups: make([]GroupOutcome, 0, len(steps))}
	for _, step := range steps {
		if out.AggregateExitCode != 0 {
			out.Groups = append(out.Groups, GroupOutcome{
				Command:    step.Run,
				ExitCode:   -1,
				StderrTail: "skipped (prior step failed)",
			})
			continue
		}
		cwd := step.Cwd
		if cwd == "" {
			cwd = worktreePath
		}
		g, err := runForeground(ctx, step.Run, cwd, repoRoot, worktreePath, slug, inheritedEnv)
		if err != nil {
			return out, err
		}
		if g.ExitCode != 0 && out.AggregateExitCode == 0 {
			out.AggregateExitCode = g.ExitCode
		}
		out.Groups = append(out.Groups, g)
	}
	return out, nil
}

// renderGroup chains `( cd <cwd> && <cmd> )` clauses with ` && ` so
// a failure short-circuits the rest of the group.
func renderGroup(steps []config.SingleStep, defaultCwd string) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		cwd := s.Cwd
		if cwd == "" {
			cwd = defaultCwd
		}
		parts = append(parts, "( cd "+shellSingleQuote(cwd)+" && "+s.Run+" )")
	}
	return strings.Join(parts, " && ")
}

// renderGroupForEntry handles the `in_container:` / `compose_service:`
// directive on a HookEntry. When neither is set, the rendering is
// identical to renderGroup. When set, each step is wrapped in
// `<engine> exec [-w cwd] <id> sh -c '<cmd>'` so the work runs
// inside the named container instead of on the host. Step.Cwd, when
// present, is interpreted relative to the container's filesystem
// and passed via `-w` (no host-side `cd` is emitted).
func renderGroupForEntry(entry config.HookEntry, defaultCwd string) (string, error) {
	if entry.Container == "" && entry.ComposeService == "" {
		return renderGroup(entry.Steps, defaultCwd), nil
	}
	engine := entry.Engine
	if engine == "" {
		engine = "docker"
	}
	id, err := containerip.ContainerID(containerip.Opts{
		Container:      entry.Container,
		ComposeService: entry.ComposeService,
		ComposeProject: entry.ComposeProject,
		Engine:         engine,
	})
	if err != nil {
		return "", fmt.Errorf("hook in_container resolve: %w", err)
	}
	parts := make([]string, 0, len(entry.Steps))
	for _, s := range entry.Steps {
		args := []string{"exec"}
		if s.Cwd != "" {
			args = append(args, "-w", s.Cwd)
		}
		args = append(args, id, "sh", "-c", s.Run)
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
// vars (GWT_MAIN, GWT_WT, TREEMAN_SLUG). Returns the *exec.Cmd so
// the caller can either reap it inline (`Wait()` on the wait=true
// path) or fire-and-forget via a detached goroutine.
func spawnDetached(cmdStr, worktreePath, repoRoot, slug, logPath string, inheritedEnv map[string]string, wait bool) (*exec.Cmd, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open hook log %s: %w", logPath, err)
	}
	defer logFile.Close()

	c := exec.Command("/bin/sh", "-c", cmdStr)
	c.Dir = worktreePath
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

func buildEnv(inheritedEnv map[string]string, repoRoot, worktreePath, slug string) []string {
	out := make([]string, 0, len(inheritedEnv)+3)
	for k, v := range inheritedEnv {
		out = append(out, k+"="+v)
	}
	out = append(out,
		"GWT_MAIN="+repoRoot,
		"GWT_WT="+worktreePath,
		"TREEMAN_SLUG="+slug,
	)
	return out
}
