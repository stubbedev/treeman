// Package hooks runs the configured hook entries for a worktree
// phase. Ported from `crates/treeman-core/src/hooks.rs`.
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
// Returns once all drivers have been forked. Logs go to
// `<worktree>/.treeman-hooks/<phase>-<group-idx>.log`.
func RunHooks(
	ctx context.Context,
	phase string,
	entries []config.HookEntry,
	repoRoot, worktreePath, slug string,
	inheritedEnv map[string]string,
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
	for i, entry := range entries {
		if len(entry.Steps) == 0 {
			continue
		}
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.log", phase, i))
		cmdStr := renderGroup(entry.Steps, worktreePath)
		pid, err := spawnDetached(cmdStr, worktreePath, repoRoot, slug, logPath, inheritedEnv)
		if err != nil {
			return out, err
		}
		out.Groups = append(out.Groups, GroupOutcome{
			Command: cmdStr,
			PID:     pid,
			LogPath: logPath,
		})
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
// a failure short-circuits the rest of the group. Mirrors the Rust
// `render_group`.
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

// shellSingleQuote wraps a string in single quotes, escaping any
// embedded ` ' ` as `'\''` per POSIX.
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
// vars (GWT_MAIN, GWT_WT, TREEMAN_SLUG).
func spawnDetached(cmdStr, worktreePath, repoRoot, slug, logPath string, inheritedEnv map[string]string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open hook log %s: %w", logPath, err)
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
		return 0, fmt.Errorf("setsid spawn `%s`: %w", cmdStr, err)
	}
	// Detach — let the daemon reap us when the child finishes via
	// SIGCHLD ignore at process start. We don't `Wait()` here.
	go func() { _ = c.Wait() }()
	return c.Process.Pid, nil
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
