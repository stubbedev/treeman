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
	// MainRoot is a fast local git probe with no upstream deadline; the
	// CLI helper chain has no ctx to thread without cascading through the
	// whole cmd tree, so Background is used here.
	root, err := gitenv.MainRoot(context.Background(), start)
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
		for i := range len(kv) {
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

// SuggestNearestCommands returns the up-to-`max` subcommand names
// in `commands` that are closest to `typed` by Levenshtein
// distance. Only entries within an edit-distance budget of 3 (or
// half the length of `typed`, whichever is larger) are returned —
// the goal is "did you mean", not "show me random commands".
//
// Aliases are considered alongside primary names so `treeman
// worktreee` still suggests `wt` if that's closer.
func SuggestNearestCommands(typed string, commands []string, maxN int) []string {
	type scored struct {
		name string
		d    int
	}
	budget := 3
	if half := len(typed) / 2; half > budget {
		budget = half
	}
	out := make([]scored, 0, len(commands))
	for _, name := range commands {
		d := levenshtein(typed, name)
		if d <= budget {
			out = append(out, scored{name, d})
		}
	}
	// Insertion sort — set is tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].d < out[j-1].d; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if maxN > 0 && len(out) > maxN {
		out = out[:maxN]
	}
	names := make([]string, len(out))
	for i, s := range out {
		names[i] = s.name
	}
	return names
}

// levenshtein returns the edit distance between a and b. Standard
// two-row DP; case-folded so `WT` vs `wt` distance is 0.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if asciiFold(a[i-1]) == asciiFold(b[j-1]) {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			m := min(sub, min(ins, del))
			curr[j] = m
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func asciiFold(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}
