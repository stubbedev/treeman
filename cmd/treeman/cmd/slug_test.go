package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stubbedev/treeman/internal/slug"
)

// TestSlugForPathUsesMainWorktreeIdentity: `treeman slug` must print the
// slug prepare and the registry use. With main_worktree enabled the main
// checkout's slug is slug.ForMain; slug.For named a linked-style wt_<hash>
// that nothing else uses. Every other path keeps slug.For.
func TestSlugForPathUsesMainWorktreeIdentity(t *testing.T) {
	repo, run := newGitRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("TREEMAN_CONFIG", "")
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "t.db"))
	linked := filepath.Join(t.TempDir(), "feat")
	run("worktree", "add", "-q", linked, "-b", "feat")
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	got := func(path, branch string) string {
		t.Helper()
		sl, err := slugForPath(ctx, path, branch)
		if err != nil {
			t.Fatalf("slugForPath(%s): %v", path, err)
		}
		return sl.Value
	}

	if want := slug.For(repo, "main").Value; got(repo, "main") != want {
		t.Errorf("main checkout without main_worktree: got %q, want %q", got(repo, "main"), want)
	}

	writeFile(t, filepath.Join(repo, ".treeman.yaml"), "main_worktree:\n  enabled: true\n")
	want := slug.ForMain(repo, "main").Value
	if want == slug.For(repo, "main").Value {
		t.Fatal("test precondition: main and linked slugs must differ")
	}
	if g := got(repo, "main"); g != want {
		t.Errorf("main checkout with main_worktree enabled: got %q, want main slug %q", g, want)
	}

	for name, path := range map[string]string{
		"linked worktree":  linked,
		"subdirectory":     sub,
		"outside any repo": t.TempDir(),
	} {
		if g, w := got(path, ""), slug.For(path, "").Value; g != w {
			t.Errorf("%s: got %q, want %q", name, g, w)
		}
	}
}
