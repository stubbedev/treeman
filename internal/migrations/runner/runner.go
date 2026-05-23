// Package runner invokes a user-declared shell command (either a
// migration runner from `migrations.migrate` or a seeder from
// `databases[].seed`) against a target database. The command and
// env-var overrides come verbatim from YAML — no framework-based
// dispatch lives here.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
)

// Spec is the shape the runner accepts: a shell command + env-var
// overrides + a label used in error messages so callers know which
// YAML block produced the failure.
type Spec struct {
	Run   string
	Env   map[string]string
	Label string // e.g. "migrations.migrate" or "seed"
}

// FromMigrate converts a `migrations.migrate` block to a Spec.
func FromMigrate(m config.MigrationMigrate) Spec {
	return Spec{Run: m.Run, Env: m.Env, Label: "migrations.migrate"}
}

// FromSeed converts a `databases[].seed` block to a Spec.
func FromSeed(s config.SeedSpec) Spec {
	return Spec{Run: s.Run, Env: s.Env, Label: "seed"}
}

// Outcome is the result of one runner invocation.
type Outcome struct {
	ExitCode   int
	StdoutTail string
	StderrTail string
}

// Run executes `spec.Run` via `sh -c` against `targetDB`.
// `inheritedEnv` is the env captured at CLI invocation (the daemon's
// PATH-aware env layered on top of its own); `spec.Env` entries are
// templated with `{target_db}` and then set last so they win.
func Run(
	ctx context.Context,
	spec Spec,
	repoRoot string,
	targetDB string,
	inheritedEnv map[string]string,
) (Outcome, error) {
	if strings.TrimSpace(spec.Run) == "" {
		label := spec.Label
		if label == "" {
			label = "command"
		}
		return Outcome{ExitCode: -1}, fmt.Errorf("%s.run is required", label)
	}
	c := exec.CommandContext(ctx, "sh", "-c", spec.Run)
	c.Dir = repoRoot

	env := make([]string, 0, len(inheritedEnv)+len(spec.Env)+1)
	for k, v := range inheritedEnv {
		env = append(env, k+"="+v)
	}
	env = append(env, "TREEMAN_TARGET_DB="+targetDB)
	for k, tmpl := range spec.Env {
		env = append(env, k+"="+strings.ReplaceAll(tmpl, "{target_db}", targetDB))
	}
	c.Env = env

	var stdout, stderr bytes.Buffer
	c.Stdout = tailWriter(&stdout, 16*1024)
	c.Stderr = tailWriter(&stderr, 16*1024)

	err := c.Run()
	out := Outcome{
		StdoutTail: stdout.String(),
		StderrTail: stderr.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
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
