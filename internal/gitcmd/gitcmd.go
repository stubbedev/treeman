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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// pathCtxKey scopes a per-call PATH override onto a context.Context.
// The daemon runs git on behalf of an interactive `treeman` invocation
// whose shell env (captured as task.InheritedEnv) holds the user's real
// PATH; treemand's own PATH under systemd --user is the stock
// `/usr/bin:/bin:…` and lacks the nix profile / login additions. Pinning
// the caller's PATH here makes daemon-spawned git — and crucially the
// clean/smudge filter git shells out (`treeman patch-filter`), plus any
// credential helper / ssh / git-invoked hook — resolve exactly as it
// would in the user's shell.
type pathCtxKey struct{}

// WithPath returns a context carrying `path` as the PATH that
// command() will use for git subprocesses spawned under it. Empty
// `path` returns ctx unchanged so callers can pass an absent
// InheritedEnv["PATH"] without branching. The override fully replaces
// PATH (not prepends) — it IS the caller's shell PATH; withSelfOnPath
// still runs afterward to guarantee treeman's own dir is present.
func WithPath(ctx context.Context, path string) context.Context {
	if path == "" {
		return ctx
	}
	return context.WithValue(ctx, pathCtxKey{}, path)
}

// pathFromCtx returns the PATH override stashed by WithPath, or "".
func pathFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(pathCtxKey{}).(string); ok {
		return v
	}
	return ""
}

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

// PipeOutput runs `git src...` and streams its stdout into the stdin of
// `git dst...`, returning dst's accumulated stdout. Both run read-only.
//
// The OS pipe between the two children means src's output is never buffered
// whole in this process — required for `git log -p <huge-range> | git patch-id`,
// where the `-p` diff of tens of thousands of commits would otherwise be held
// in memory all at once. dst's stdout (patch-id's one-line-per-commit summary)
// is small, so collecting it in a buffer is safe and can't deadlock the pipe.
func PipeOutput(ctx context.Context, dir string, src, dst []string) ([]byte, error) {
	srcCmd := command(ctx, dir, true, src...)
	dstCmd := command(ctx, dir, true, dst...)

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("git pipe: %w", err)
	}
	srcCmd.Stdout = pw
	dstCmd.Stdin = pr

	var out, srcErr, dstErr bytes.Buffer
	srcCmd.Stderr = &srcErr
	dstCmd.Stdout = &out
	dstCmd.Stderr = &dstErr

	if err := dstCmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, wrap(dst, err, dstErr.String())
	}
	if err := srcCmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		_ = dstCmd.Wait()
		return nil, wrap(src, err, srcErr.String())
	}
	// Both children hold their own dup of the pipe ends now; drop this
	// process's copies so dst sees EOF once src exits and closes its write end.
	_ = pw.Close()
	_ = pr.Close()

	srcWait := srcCmd.Wait()
	dstWait := dstCmd.Wait()
	if srcWait != nil {
		return nil, wrap(src, srcWait, srcErr.String())
	}
	if dstWait != nil {
		return nil, wrap(dst, dstWait, dstErr.String())
	}
	return out.Bytes(), nil
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
	if p := pathFromCtx(ctx); p != "" {
		env = setEnvKey(env, "PATH", p)
	}
	env = withSelfOnPath(env)
	cmd.Env = env
	return cmd
}

// setEnvKey replaces (or appends) `name=val` in `env`, returning the
// updated slice. Later entries in a process env win, but rewriting in
// place keeps the env compact and avoids relying on that ordering
// quirk for PATH — which some libc getenv paths read first-match.
func setEnvKey(env []string, name, val string) []string {
	prefix := name + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// withSelfOnPath prepends the running executable's directory to PATH
// in `env`. Git invokes treeman's clean/smudge filter as the bare
// program `treeman patch-filter` (see internal/patcher/install.go),
// resolving it through the PATH of whatever process spawned git. When
// that process is treemand running under systemd --user — whose PATH
// is the stock `/usr/bin:/bin:…` and does NOT inherit the user's nix
// profile or login shell — the bare `treeman` can't be found and the
// filter fails with exit 127. Pinning our own bin dir onto the child
// PATH guarantees the filter resolves to the SAME binary that's
// driving the git operation, regardless of how the daemon was
// launched. Best-effort: if os.Executable() fails, env is unchanged.
func withSelfOnPath(env []string) []string {
	exe, err := os.Executable()
	if err != nil {
		return env
	}
	dir := filepath.Dir(exe)
	if dir == "" {
		return env
	}
	for i, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || name != "PATH" {
			continue
		}
		// Skip if our dir is already the first PATH entry — avoids
		// unbounded growth across nested git invocations that inherit
		// this same env.
		if val == dir || strings.HasPrefix(val, dir+string(os.PathListSeparator)) {
			return env
		}
		env[i] = "PATH=" + dir + string(os.PathListSeparator) + val
		return env
	}
	// No PATH in env at all — add one.
	return append(env, "PATH="+dir)
}

func wrap(args []string, err error, stderr string) error {
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return &Error{
		Args:     args,
		ExitCode: code,
		Stderr:   stderr,
		Err:      err,
	}
}
