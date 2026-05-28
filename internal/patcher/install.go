package patcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/gitcmd"
)

// FilterName is the git filter identifier wired into `.git/config`
// and `info/attributes`. Stable: changing it would orphan filter
// config written by earlier treeman versions, leaving worktrees
// half-configured.
const FilterName = "treeman-patch"

// attrsHeader marks the treeman-managed block in `info/attributes`.
// EnsureFilter strips any existing block before appending so
// repeated invocations don't leave stale lines for removed patches.
const attrsHeader = "# treeman-managed: clean/smudge for patches: DO NOT EDIT — regenerated on every finalize"

// EnsureFilter wires git's clean/smudge for `files` in this worktree:
//
//   - Configures filter.<FilterName>.{clean,smudge,required} via
//     `git config --local` so the filter program is bound to this
//     repo's `.git/config` (not the user's global, where it could
//     leak into unrelated repos).
//   - Writes `info/attributes` under the shared git common dir
//     (`<repo>/.git/info/attributes`) listing each patched path
//     with `filter=<FilterName>`. Git only consults the common-dir
//     `info/attributes` — a per-linked-worktree copy is silently
//     ignored (verified empirically on git 2.54). Common-dir bleed
//     into the main worktree and sibling linked worktrees is
//     harmless: the filter program (`treeman patch-filter`)
//     resolves config from cwd and falls through as pass-through
//     for any file that the current worktree doesn't actually
//     patch (see filter.go fallback).
//   - Clears any legacy `--skip-worktree` bit on patched files so
//     `git pull` no longer refuses to overwrite them. Filter mode
//     supersedes skip-worktree entirely.
//
// Idempotent: re-running rewrites the treeman block in
// info/attributes from scratch, and `git config` overwrites existing
// values. `files` are relative to the worktree root.
func EnsureFilter(ctx context.Context, worktreePath string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	// Resolve the shared git common dir. Git reads `info/attributes`
	// from $GIT_COMMON_DIR for every worktree (main + linked); a
	// per-worktree `<GIT_DIR>/info/attributes` is not consulted, so
	// writing there leaves the filter unwired and `git status` shows
	// patched files as modified.
	gd, err := gitcmd.String(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("rev-parse --git-common-dir: %w", err)
	}
	gitDir := strings.TrimSpace(gd)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreePath, gitDir)
	}

	if err := ensureFilterConfig(ctx, worktreePath); err != nil {
		return err
	}
	if err := writeAttributes(gitDir, files); err != nil {
		return err
	}
	// Best-effort legacy migration: clear --skip-worktree on any
	// patched file that still carries it from treeman < 2.5.
	// Failure is not fatal — the worst case is the file stays
	// pinned and the user clears it manually.
	tracked := make([]string, 0, len(files))
	for _, f := range files {
		abs := filepath.Join(worktreePath, f)
		if err := gitcmd.RunOptional(ctx, worktreePath, "ls-files", "--error-unmatch", abs); err != nil {
			continue // untracked file — no index entry to clear
		}
		_, _ = gitcmd.OutputRW(ctx, worktreePath, false, "update-index", "--no-skip-worktree", abs)
		tracked = append(tracked, f)
	}

	// Push the filter-cleaned content into the index so git's pull /
	// merge safety check sees the working tree as matching HEAD for
	// patched keys. Without this step the stat-cache stays primed
	// with the patched form and `git pull` refuses with "Your local
	// changes would be overwritten by merge" — defeating the whole
	// point of the switch. Errors are non-fatal (e.g. filter program
	// unreachable, file gitignored) — caller logs and continues.
	for _, f := range tracked {
		args := []string{"add", "--renormalize", "--", f}
		_, _ = gitcmd.OutputRW(ctx, worktreePath, false, args...)
	}
	return nil
}

// ensureFilterConfig writes the filter program lines into the
// per-worktree git config so git invokes `treeman patch-filter` for
// the relevant attribute matches. Values are idempotent: re-runs
// just overwrite the same keys.
//
// `treeman` is resolved via PATH on each filter invocation — using
// an absolute path here would break for users who reinstall treeman
// somewhere else without re-running finalize. The contract is
// "treeman must be on PATH for git operations in patched
// worktrees"; documented in the release notes.
func ensureFilterConfig(ctx context.Context, worktreePath string) error {
	pairs := [][2]string{
		{"filter." + FilterName + ".clean", "treeman patch-filter clean %f"},
		{"filter." + FilterName + ".smudge", "treeman patch-filter smudge %f"},
		{"filter." + FilterName + ".required", "true"},
	}
	for _, kv := range pairs {
		if _, err := gitcmd.OutputRW(ctx, worktreePath, false, "config", "--local", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git config %s=%s: %w", kv[0], kv[1], err)
		}
	}
	return nil
}

// writeAttributes rewrites `<gitCommonDir>/info/attributes`,
// replacing the treeman-managed block (between attrsHeader and the
// next blank line / EOF) with one filter=<FilterName> line per file.
// Other users' attributes are left untouched. Caller must pass the
// common dir (e.g. from `git rev-parse --git-common-dir`); the
// per-linked-worktree GIT_DIR is not consulted by git for
// `info/attributes`.
func writeAttributes(gitDir string, files []string) error {
	infoDir := filepath.Join(gitDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", infoDir, err)
	}
	attrPath := filepath.Join(infoDir, "attributes")
	body, _ := os.ReadFile(attrPath)

	var out strings.Builder
	skipping := false
	for _, line := range strings.Split(string(body), "\n") {
		if line == attrsHeader {
			skipping = true
			continue
		}
		if skipping {
			if strings.TrimSpace(line) == "" {
				skipping = false
			}
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	tail := out.String()
	tail = strings.TrimRight(tail, "\n")
	if tail != "" {
		tail += "\n\n"
	}
	tail += attrsHeader + "\n"
	for _, f := range files {
		// Quote paths with whitespace; otherwise git treats only the
		// first token as the pattern. Plain quoting (no escape) is
		// fine — gitattributes uses the same pattern syntax as
		// gitignore for path matching.
		pattern := f
		if strings.ContainsAny(pattern, " \t") {
			pattern = `"` + pattern + `"`
		}
		tail += pattern + " filter=" + FilterName + "\n"
	}
	tail += "\n"
	return os.WriteFile(attrPath, []byte(tail), 0o644)
}
