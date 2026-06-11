// Package runner invokes a user-declared shell command (either a
// migration runner from `migrations.migrate` or a seeder from
// `databases[].seed`) against a target database. The command and
// env-var overrides come verbatim from YAML — no framework-based
// dispatch lives here.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/shellenv"
	"github.com/stubbedev/treeman/internal/template"
)

// Spec is the shape the runner accepts: a shell command + env-var
// overrides + a label used in error messages so callers know which
// YAML block produced the failure.
//
// LogPath (optional): when non-empty, the merged stdout+stderr stream
// is teed to this file so a post-mortem can read the full output after
// the in-memory tails roll over. Parent directories are created. The
// file is truncated on each invocation — callers wanting history
// should rotate the path themselves.
type Spec struct {
	Run     string
	Env     map[string]string
	Label   string // e.g. "migrations.migrate" or "seed"
	LogPath string
}

// FromMigrate converts a `databases[].migrate` block to a Spec.
func FromMigrate(s config.Step) Spec {
	return Spec{Run: s.Run, Env: s.Env, Label: "migrate"}
}

// FromSeed converts a `databases[].seed` block to a Spec.
func FromSeed(s config.Step) Spec {
	return Spec{Run: s.Run, Env: s.Env, Label: "seed"}
}

// WithLogPath returns a copy of the Spec with LogPath set. Lets call
// sites stay fluent without mutating the source Spec.
func (s Spec) WithLogPath(p string) Spec {
	s.LogPath = p
	return s
}

// Outcome is the result of one runner invocation.
type Outcome struct {
	ExitCode   int
	StdoutTail string
	StderrTail string
	LogPath    string // mirror of Spec.LogPath; empty when no log was teed
}

// FormatError builds the canonical "<label> <target> exit N: …" string
// used by every prepare call site. Includes both stderr and stdout tails
// because frameworks like Laravel/Symfony Console write failure
// diagnostics to stdout, not stderr — earlier versions formatted only
// StderrTail and turned exit 1 into an empty diagnostic. Trailing
// whitespace is trimmed and empty streams are skipped. When a LogPath
// was teed, append a pointer to the full log.
func FormatError(label, target string, out Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s exit %d", label, target, out.ExitCode)
	stderrTrim := strings.TrimSpace(out.StderrTail)
	stdoutTrim := strings.TrimSpace(out.StdoutTail)
	wrote := false
	if stderrTrim != "" {
		fmt.Fprintf(&b, ": stderr=%q", stderrTrim)
		wrote = true
	}
	if stdoutTrim != "" {
		if wrote {
			b.WriteString(" ")
		} else {
			b.WriteString(": ")
		}
		fmt.Fprintf(&b, "stdout=%q", stdoutTrim)
		wrote = true
	}
	if !wrote {
		b.WriteString(": <no output captured>")
	}
	if out.LogPath != "" {
		fmt.Fprintf(&b, " (full log: %s)", out.LogPath)
	}
	return b.String()
}

// Run executes `spec.Run` via `sh -c` against `targetDB`.
// `inheritedEnv` is the env captured at CLI invocation (the daemon's
// PATH-aware env layered on top of its own); `spec.Env` values are
// rendered through `template.Render` with `tplCtx.WithTargetDB(targetDB)`
// — the same template pass that already produced `targetDB` — so they
// can reference every supported placeholder (`{target_db}`, `{slug}`,
// `{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`, `{n}`)
// and a typo like `{target-db}` fails loud instead of being passed
// through to the subprocess. Rendered env entries are appended last
// so they win over the inherited base.
func Run(
	ctx context.Context,
	spec Spec,
	repoRoot string,
	targetDB string,
	tplCtx template.Context,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if strings.TrimSpace(spec.Run) == "" {
		label := spec.Label
		if label == "" {
			label = "command"
		}
		return Outcome{ExitCode: -1}, fmt.Errorf("%s.run is required", label)
	}
	tplCtx = tplCtx.WithTargetDB(targetDB)
	renderedEnv := make(map[string]string, len(spec.Env))
	for k, tmpl := range spec.Env {
		v, err := template.Render(tmpl, tplCtx)
		if err != nil {
			return Outcome{ExitCode: -1}, fmt.Errorf("%s.env[%s]: %w", spec.Label, k, err)
		}
		renderedEnv[k] = v
	}

	c := exec.CommandContext(ctx, "sh", "-c", spec.Run)
	c.Dir = repoRoot

	// Build the subprocess env via the same model as hooks: daemon
	// floor, overlaid with the user's cached env, with the login-shell
	// PATH always merged in (see shellenv.BaseEnv). This gives migrate/
	// seed commands the same env they'd have in the user's own terminal,
	// and keeps them working when inheritedEnv is empty (an externally-
	// created worktree finalized by the lifecycle watcher). Rendered env
	// and TREEMAN_TARGET_DB are layered last so they win.
	merged := shellenv.BaseEnv(inheritedEnv)
	merged["TREEMAN_TARGET_DB"] = targetDB
	maps.Copy(merged, renderedEnv)
	env := make([]string, 0, len(merged))
	for k, v := range merged {
		env = append(env, k+"="+v)
	}
	c.Env = env

	var stdout, stderr bytes.Buffer
	var stdoutW io.Writer = tailWriter(&stdout, 16*1024)
	var stderrW io.Writer = tailWriter(&stderr, 16*1024)

	if spec.LogPath != "" {
		if mkErr := os.MkdirAll(filepath.Dir(spec.LogPath), 0o755); mkErr != nil {
			return Outcome{ExitCode: -1}, fmt.Errorf("%s log dir: %w", spec.Label, mkErr)
		}
		f, fErr := os.Create(spec.LogPath)
		if fErr != nil {
			return Outcome{ExitCode: -1}, fmt.Errorf("%s log open: %w", spec.Label, fErr)
		}
		defer func() { _ = f.Close() }()
		stdoutW = io.MultiWriter(stdoutW, f)
		stderrW = io.MultiWriter(stderrW, f)
	}
	c.Stdout = stdoutW
	c.Stderr = stderrW

	err := c.Run()
	out := Outcome{
		StdoutTail: stdout.String(),
		StderrTail: stderr.String(),
		LogPath:    spec.LogPath,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		out.ExitCode = exitErr.ExitCode()
		return out, nil
	}
	if err != nil {
		out.ExitCode = -1
		return out, err
	}
	out.ExitCode = 0
	return out, nil
}

// tailWriter caps a bytes.Buffer at `n` bytes by truncating the
// front when growing. Cheap and lossy on the head; good enough for
// driver logs.
func tailWriter(buf *bytes.Buffer, n int) *capWriter {
	return &capWriter{buf: buf, cap: n}
}

type capWriter struct {
	buf *bytes.Buffer
	cap int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if _, err := w.buf.Write(p); err != nil {
		return 0, err
	}
	if w.buf.Len() > w.cap {
		drop := w.buf.Len() - w.cap
		w.buf.Next(drop)
	}
	return len(p), nil
}
