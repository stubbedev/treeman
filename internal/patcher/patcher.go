// Package patcher applies (key,value) pairs to dotenv-style files
// and phpunit.xml `<env>` blocks.
//
// `SkipWorktree` calls `git update-index --skip-worktree` so the
// per-worktree patched `.env.testing` doesn't show up as a dirty
// file. Re-pull of the file (e.g. after a `git pull` that changes
// it upstream) requires the user to manually
// `git update-index --no-skip-worktree` first.
package patcher

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Outcome reports what the patch call did.
type Outcome int

const (
	// Updated — file content changed and was written.
	Updated Outcome = iota
	// Unchanged — file already had the requested values.
	Unchanged
	// Missing — file does not exist; nothing written.
	Missing
)

// Pair is a (key, value) tuple. Slices of these drive the patcher.
type Pair struct {
	Key   string
	Value string
}

// PatchEnvFile rewrites a dotenv-style file at `path`. Existing
// `KEY=...` lines are replaced in place; missing keys appended at
// the end with a trailing newline.
func PatchEnvFile(path string, pairs []Pair) (Outcome, error) {
	original, err := readFile(path)
	if err != nil {
		return Missing, nil
	}
	content := original
	for _, p := range pairs {
		content = patchEnvOne(content, p.Key, p.Value)
	}
	if content == original {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

func patchEnvOne(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `\s*=.*$`)
	if re.MatchString(content) {
		return re.ReplaceAllString(content, key+"="+value)
	}
	out := content
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += key + "=" + value + "\n"
	return out
}

// PatchPhpunitFile rewrites a phpunit.xml file at `path`. Existing
// `<env name="KEY" .../>` entries are replaced in place; missing
// entries inserted before `</php>` with the same `\t...\n\t</php>`
// indent the bash hook produced (preserves diff parity).
func PatchPhpunitFile(path string, pairs []Pair) (Outcome, error) {
	original, err := readFile(path)
	if err != nil {
		return Missing, nil
	}
	content := original
	for _, p := range pairs {
		content = patchPhpunitOne(content, p.Key, p.Value)
	}
	if content == original {
		return Unchanged, nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Updated, fmt.Errorf("write %s: %w", path, err)
	}
	return Updated, nil
}

func patchPhpunitOne(content, key, value string) string {
	re := regexp.MustCompile(`<env name="` + regexp.QuoteMeta(key) + `"[^/]*/>`)
	replacement := fmt.Sprintf(`<env name="%s" value="%s" force="true"/>`, key, value)
	if re.MatchString(content) {
		return re.ReplaceAllString(content, replacement)
	}
	inject := "\t" + replacement + "\n\t</php>"
	return strings.Replace(content, "</php>", inject, 1)
}

// SkipWorktree shells out to `git update-index --skip-worktree
// <file>` from `gitDir`. Returns false (no error) if the file is
// not tracked by git — typical for `.env.testing` which is
// gitignored.
//
// `gitDir` MUST be the worktree the file lives in, not the main
// repo. Linked worktrees have a per-worktree index; running git
// from the main repo would update the wrong index (and `ls-files`
// from there wouldn't even find the file). Idempotent: re-running
// against an already-skipped file exits 0 with no change.
//
// We shell out instead of pulling in a libgit2 binding because git
// is already required by treeman anyway.
func SkipWorktree(gitDir, file string) (bool, error) {
	// Probe first: `git ls-files --error-unmatch <file>` exits 0
	// when the path is tracked, non-zero otherwise.
	probe := exec.Command("git", "-C", gitDir, "ls-files", "--error-unmatch", file)
	probe.Stdout, probe.Stderr = nil, nil
	if err := probe.Run(); err != nil {
		return false, nil
	}
	cmd := exec.Command("git", "-C", gitDir, "update-index", "--skip-worktree", file)
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("git update-index --skip-worktree %s: %w", file, err)
	}
	return true, nil
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
