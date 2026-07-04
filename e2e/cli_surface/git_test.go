//go:build e2e

package cli_surface_e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitSurface exercises the non-interactive paths of `treeman git`.
// The picker/TUI paths need a TTY and are covered by the model-level
// tests in internal/tui; here we pin exit codes, stdout shape, and the
// non-TTY degradations (commit-from-args, friendly picker errors).
func TestGitSurface(t *testing.T) {
	e := newEnv(t)

	write := func(t *testing.T, repo, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("status shows short status", func(t *testing.T) {
		repo := newGitRepo(t)
		write(t, repo, "dirty.txt", "x\n")
		res := e.run(t, repo, "git", "status")
		if res.err != nil {
			t.Fatalf("git status: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if !strings.Contains(res.stdout, "?? dirty.txt") {
			t.Errorf("stdout missing untracked marker:\n%s", res.stdout)
		}
	})

	t.Run("commit non-TTY uses args as message with ticket prefix", func(t *testing.T) {
		repo := newGitRepo(t)
		mustGit(t, repo, "checkout", "-q", "-b", "feature/KON-42-thing")
		write(t, repo, "f.txt", "x\n")
		mustGit(t, repo, "add", "-A")
		res := e.run(t, repo, "git", "commit", "add", "the", "thing")
		if res.err != nil {
			t.Fatalf("git commit: %v\nstderr:\n%s", res.err, res.stderr)
		}
		out := gitOut(t, repo, "log", "-1", "--format=%s")
		if got := strings.TrimSpace(out); got != "KON-42: add the thing" {
			t.Errorf("subject = %q, want ticket-prefixed", got)
		}
	})

	t.Run("commit non-TTY without args errors with hint", func(t *testing.T) {
		repo := newGitRepo(t)
		write(t, repo, "f.txt", "x\n")
		mustGit(t, repo, "add", "-A")
		res := e.run(t, repo, "git", "commit")
		if res.err == nil {
			t.Fatal("expected error without a TTY or args")
		}
		if !strings.Contains(res.stderr, "pass the message") {
			t.Errorf("stderr missing hint:\n%s", res.stderr)
		}
	})

	t.Run("switch existing branch prints path and checks out", func(t *testing.T) {
		repo := newGitRepo(t)
		mustGit(t, repo, "branch", "other")
		res := e.run(t, repo, "git", "switch", "other")
		if res.err != nil {
			t.Fatalf("git switch: %v\nstderr:\n%s", res.err, res.stderr)
		}
		// stdout must be exactly the destination path (the cd contract).
		if got := strings.TrimSpace(res.stdout); got != repo {
			t.Errorf("stdout = %q, want repo path %q", got, repo)
		}
		if b := strings.TrimSpace(gitOut(t, repo, "branch", "--show-current")); b != "other" {
			t.Errorf("branch = %q, want other", b)
		}
	})

	t.Run("switch new branch creates it", func(t *testing.T) {
		repo := newGitRepo(t)
		res := e.run(t, repo, "git", "switch", "feature/fresh")
		if res.err != nil {
			t.Fatalf("git switch: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if b := strings.TrimSpace(gitOut(t, repo, "branch", "--show-current")); b != "feature/fresh" {
			t.Errorf("branch = %q, want feature/fresh", b)
		}
	})

	t.Run("switch no-arg without TTY errors", func(t *testing.T) {
		repo := newGitRepo(t)
		res := e.run(t, repo, "git", "switch")
		if res.err == nil {
			t.Fatal("expected error: picker needs a terminal")
		}
		if !strings.Contains(res.stderr, "terminal") {
			t.Errorf("stderr missing terminal hint:\n%s", res.stderr)
		}
	})

	t.Run("diff guards", func(t *testing.T) {
		repo := newGitRepo(t)
		if res := e.run(t, repo, "git", "diff"); res.err != nil {
			t.Errorf("plain diff should exit 0: %v\n%s", res.err, res.stderr)
		}
		if res := e.run(t, repo, "git", "diff", "--patch"); res.err == nil {
			t.Error("--patch without a branch should error")
		}
		res := e.run(t, repo, "git", "diff", "no-such-branch")
		if res.err == nil || !strings.Contains(res.stderr, "no branch") {
			t.Errorf("want friendly no-branch error, got err=%v stderr:\n%s", res.err, res.stderr)
		}
	})

	t.Run("undo soft-resets keeping changes staged", func(t *testing.T) {
		repo := newGitRepo(t)
		write(t, repo, "f.txt", "x\n")
		mustGit(t, repo, "add", "-A")
		mustGit(t, repo, "commit", "-q", "-m", "second")
		res := e.run(t, repo, "git", "undo")
		if res.err != nil {
			t.Fatalf("git undo: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if subj := strings.TrimSpace(gitOut(t, repo, "log", "-1", "--format=%s")); subj != "init" {
			t.Errorf("HEAD subject = %q, want init", subj)
		}
		staged := gitOut(t, repo, "diff", "--cached", "--name-only")
		if !strings.Contains(staged, "f.txt") {
			t.Errorf("f.txt not staged after undo:\n%s", staged)
		}
	})

	t.Run("wipe non-TTY discards changes", func(t *testing.T) {
		repo := newGitRepo(t)
		write(t, repo, "junk.txt", "x\n")
		// Non-TTY Confirm auto-proceeds (documented opt-in for scripts).
		res := e.run(t, repo, "git", "wipe")
		if res.err != nil {
			t.Fatalf("git wipe: %v\nstderr:\n%s", res.err, res.stderr)
		}
		if st := strings.TrimSpace(gitOut(t, repo, "status", "--porcelain")); st != "" {
			t.Errorf("worktree not clean after wipe:\n%s", st)
		}
	})

	t.Run("interactive-only verbs error without TTY", func(t *testing.T) {
		repo := newGitRepo(t)
		write(t, repo, "pending.txt", "x\n") // give `add` something to offer
		for _, argv := range [][]string{{"git", "log"}, {"git", "add"}} {
			res := e.run(t, repo, argv...)
			if res.err == nil {
				t.Errorf("%v: expected non-TTY error", argv)
			}
		}
	})

	t.Run("add on clean tree is a no-op exit 0", func(t *testing.T) {
		repo := newGitRepo(t)
		res := e.run(t, repo, "git", "add")
		if res.err != nil {
			t.Errorf("clean-tree add should exit 0: %v\n%s", res.err, res.stderr)
		}
	})
}

// gitOut runs git in dir and returns stdout, failing the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
