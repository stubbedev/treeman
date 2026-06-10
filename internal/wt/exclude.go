package wt

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// excludeHeader marks the block treeman appends to info/exclude, so a
// human reading the file knows where the brought-in patterns come from.
const excludeHeader = "# treeman: worktrees.links / worktrees.copies (brought-in, kept out of git status)"

// ensureRepoExcludes adds anchored ignore patterns for brought-in
// links/copies to the repository's shared exclude file
// (<commonDir>/info/exclude), so the symlinked/copied paths never surface
// as untracked entries ("?? path") that make a worktree look dirty.
//
// git resolves info/exclude from the *common* git dir, shared by every
// linked worktree, so one anchored entry (e.g. "/.claude") suppresses the
// same path in every worktree and the main checkout. Anchoring with a
// leading slash pins the rule to the repo root, so an unrelated nested
// file of the same name is still reported. Exclude rules only affect
// untracked files, so a pattern for a tracked path (e.g. a committed
// justfile) is a harmless no-op.
//
// Any returned error is for the caller to log as a warning — failing to
// hide a file must never fail the bring-in itself.
func ensureRepoExcludes(repoRoot string, rels []string) error {
	patterns := anchoredPatterns(rels)
	if len(patterns) == 0 {
		return nil
	}

	commonDir, err := gitCommonDir(repoRoot)
	if err != nil {
		return err
	}

	excludePath := filepath.Join(commonDir, "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}

	f, err := os.OpenFile(excludePath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// A CLI sync and the daemon finalize run in different processes and can
	// touch this shared file at once; lock the whole read-modify-write.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()

	body, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	present := map[string]bool{}
	hasHeader := false
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		present[trimmed] = true
		if trimmed == excludeHeader {
			hasHeader = true
		}
	}

	var add []string
	for _, p := range patterns {
		if present[p] {
			continue
		}
		present[p] = true
		add = append(add, p)
	}
	if len(add) == 0 {
		return nil
	}

	var b strings.Builder
	if len(body) > 0 && !strings.HasSuffix(string(body), "\n") {
		b.WriteByte('\n')
	}
	if !hasHeader {
		b.WriteString(excludeHeader)
		b.WriteByte('\n')
	}
	for _, p := range add {
		b.WriteString(p)
		b.WriteByte('\n')
	}

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	return err
}

// anchoredPatterns turns repo-relative paths into deduped, root-anchored
// gitignore patterns ("/dir/file"), dropping empties, "." and "../" escapes.
func anchoredPatterns(rels []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, rel := range rels {
		rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
		rel = strings.Trim(rel, "/")
		if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		p := "/" + rel
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// gitCommonDir returns the directory holding info/exclude for the repo at
// repoRoot. For the main checkout that's <repoRoot>/.git (a real dir); a
// ".git" gitlink file is resolved via its "gitdir:" pointer plus the
// gitdir's "commondir" file, so a linked worktree still lands on the
// shared exclude.
func gitCommonDir(repoRoot string) (string, error) {
	dotGit := filepath.Join(repoRoot, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return dotGit, nil
	}

	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", err
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
	if gitDir == "" {
		return "", fmt.Errorf("no gitdir pointer in %s", dotGit)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoRoot, gitDir)
	}
	if cd, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		return filepath.Clean(common), nil
	}
	return gitDir, nil
}
