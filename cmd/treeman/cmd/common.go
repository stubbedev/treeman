// Package cmd holds the urfave/cli subcommand handlers for the
// treeman CLI. Each file is one subcommand surface; helpers live
// in this file.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stubbedev/treeman/internal/gitenv"
	"github.com/stubbedev/treeman/internal/ui"
)

// DiscoverRepoRoot returns the MAIN repo root for `start`. When
// `start` is inside a linked worktree, `gitenv.MainRoot` resolves
// the gitlink → `<main>/.git` → `<main>`, so callers always see
// the checkout that owns `.treeman.yaml` and the seed dump.
func DiscoverRepoRoot(start string) (string, error) {
	root, err := gitenv.MainRoot(start)
	if err != nil {
		return "", fmt.Errorf("discover repo root from %s: %w", start, err)
	}
	return root, nil
}

// CaptureInheritedEnv snapshots os.Environ() into a BTreeMap-shaped
// map[string]string. Sent over the RPC wire so the daemon's hook +
// migrate subprocesses see the interactive shell's $PATH.
func CaptureInheritedEnv() map[string]string {
	env := os.Environ()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				out[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return out
}

// MustAbs canonicalises a path or fails. Used to keep cmd handlers
// terse.
func MustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// PrintOK / PrintWarn delegate to the ui package's colored helpers.
// Color auto-disables on pipes, NO_COLOR=1, or dumb terminals — see
// internal/ui.ColorEnabled.
func PrintOK(format string, args ...any)   { ui.Success(format, args...) }
func PrintWarn(format string, args ...any) { ui.Warn(format, args...) }
func PrintErr(format string, args ...any)  { ui.Error(format, args...) }
func PrintInfo(format string, args ...any) { ui.Info(format, args...) }
func PrintHint(format string, args ...any) { ui.Hint(format, args...) }

// Ctx returns a background context (no per-CLI cancellation today).
func Ctx() context.Context { return context.Background() }
