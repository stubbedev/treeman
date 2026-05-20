// Package cmd holds the urfave/cli subcommand handlers for the
// treeman CLI. Each file is one subcommand surface; helpers live
// in this file.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DiscoverRepoRoot walks up from `start` until it finds a `.git`
// directory or a `.treeman.yaml` file. Returns "" + error when
// neither is present in any ancestor.
func DiscoverRepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".treeman.yaml")); err == nil && !fi.IsDir() {
			return dir, nil
		}
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			_ = fi
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .git or .treeman.yaml in any ancestor of %s", start)
		}
		dir = parent
	}
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

// PrintOK / PrintWarn / PrintErr are convenience println helpers
// with consistent prefixes (no ANSI colour for portability).
func PrintOK(format string, args ...any) { fmt.Println("ok: " + fmt.Sprintf(format, args...)) }
func PrintWarn(format string, args ...any) {
	fmt.Fprintln(os.Stderr, "warn: "+fmt.Sprintf(format, args...))
}

// Ctx returns a background context (no per-CLI cancellation today).
func Ctx() context.Context { return context.Background() }
