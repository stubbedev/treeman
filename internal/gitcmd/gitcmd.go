// Package gitcmd centralises `git` subprocess invocations so every
// call site applies the same environment hygiene + error wrapping.
//
// Why a wrapper instead of `exec.CommandContext` calls everywhere:
//
//   - GIT_TERMINAL_PROMPT=0 is set unconditionally. Without it a
//     misconfigured credential helper (or `git fetch` against a
//     private repo) blocks the CLI forever waiting on a TTY prompt
//     that never comes when the caller is the daemon, MCP server,
//     or any non-interactive context.
//   - GIT_OPTIONAL_LOCKS=0 is set for read-only ops. Skips the
//     index-update side effects that real-world `git status` /
//     `git worktree list` would do, which is wasted I/O on large
//     repos and contends with concurrent writers.
//   - Stderr is captured and returned alongside any exec error so
//     the wrapping message includes git's actual complaint instead
//     of a bare `exit status 128`.
//   - Every helper requires a context.Context — cancellation works
//     end to end without each call site remembering to pass one.
//
// All helpers shell to `git` on PATH. We don't import a Go git
// library: treeman already requires git to be installed (it
// manages git worktrees), so the subprocess cost is irrelevant and
// the user's git binary handles every edge case (signed commits,
// sparse checkout, fsmonitor, protocol v2, ...) that a library
// would otherwise have to chase.
package gitcmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Error wraps an exec failure with the command + stderr tail so
// callers' error messages include git's actual complaint.
type Error struct {
	Args     []string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *Error) Error() string {
	tail := strings.TrimSpace(e.Stderr)
	if len(tail) > 400 {
		tail = "…" + tail[len(tail)-400:]
	}
	if tail != "" {
		return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, tail)
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Run executes `git args...` against `dir` and returns nil on
// exit-0. Stderr is captured into the returned error on failure.
// Stdin is /dev/null.
func Run(ctx context.Context, dir string, args ...string) error {
	_, err := Output(ctx, dir, args...)
	return err
}

// RunOptional is like Run but the *Error result on non-zero exit is
// returned silently — useful for `rev-parse --verify` style probes
// where a non-zero exit is the expected "ref doesn't exist" signal.
// Caller decides via err == nil / *Error{ExitCode: ...} how to
// interpret.
func RunOptional(ctx context.Context, dir string, args ...string) error {
	cmd := command(ctx, dir, false, args...)
	cmd.Stdin = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	return wrap(args, err, stderr.String())
}

// Output runs `git args...` and returns stdout bytes. Trailing
// newline is preserved — callers that want a trimmed string use
// String. `readOnly` defaults to true; pass true for queries (sets
// GIT_OPTIONAL_LOCKS=0) and false for ops that mutate refs / the
// index.
func Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return OutputRW(ctx, dir, true, args...)
}

// OutputRW is the read/write-aware variant. Sets
// GIT_OPTIONAL_LOCKS=0 only when readOnly is true so a write
// operation (commit / worktree add) doesn't silently skip the lock
// it needs.
func OutputRW(ctx context.Context, dir string, readOnly bool, args ...string) ([]byte, error) {
	cmd := command(ctx, dir, readOnly, args...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, wrap(args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// String is Output + TrimSpace. Convenience for parsers that expect
// a single line (rev-parse, symbolic-ref).
func String(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := Output(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// RunPiped runs `git args...` with stdout + stderr wired to the
// supplied writers. Used by the user-facing `git worktree add` /
// `git fetch` calls so progress streams to the user's terminal.
// Both writers default to io.Discard when nil.
func RunPiped(ctx context.Context, dir string, stdout, stderr io.Writer, args ...string) error {
	cmd := command(ctx, dir, false, args...)
	cmd.Stdin = nil
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return wrap(args, err, "")
	}
	return nil
}

// Exists returns true when `git rev-parse --verify --quiet <ref>`
// succeeds in `dir`. Used by branch / remote-ref existence probes.
func Exists(ctx context.Context, dir, ref string) bool {
	return RunOptional(ctx, dir, "rev-parse", "--verify", "--quiet", ref) == nil
}

// command builds the exec.Cmd with the standard env scrubbing applied.
func command(ctx context.Context, dir string, readOnly bool, args ...string) *exec.Cmd {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)

	env := os.Environ()
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if readOnly {
		env = append(env, "GIT_OPTIONAL_LOCKS=0")
	}
	cmd.Env = env
	return cmd
}

func wrap(args []string, err error, stderr string) error {
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	}
	return &Error{
		Args:     args,
		ExitCode: code,
		Stderr:   stderr,
		Err:      err,
	}
}
