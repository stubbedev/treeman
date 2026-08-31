//go:build e2e

// clean_status_test drives the REAL clean filter (`treeman patch-filter
// clean`, the binary git invokes) across every driver and asserts that a
// patched, tracked file stays CLEAN in `git status` — i.e. treeman's
// per-worktree rewrites never trip git's dirty checks.
//
// patches_test.go proves the rewrite lands; restore_byte_stable_test
// (unit) proves Clean() is byte-stable per driver; pull_filter_test
// proves git pull doesn't refuse. This test closes the loop: the actual
// installed filter program + real git, all six formats, status clean.
package patches_e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/patcher"
	"github.com/stubbedev/treeman/internal/resolve"
	"github.com/stubbedev/treeman/internal/slug"
	"github.com/stubbedev/treeman/internal/template"
)

func TestCleanFilterKeepsGitStatusClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	ctx := context.Background()

	// Build treeman and put it on PATH: git invokes the filter as the
	// bare program `treeman patch-filter clean` (required=true), so it
	// must resolve for every git op in the patched worktree.
	projectRoot := mustProjectRoot(t)
	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "treeman"), "./cmd/treeman")
	build.Dir = projectRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build treeman: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Isolate the store the filter opens for its template context, and
	// the global config, away from the developer's real ones.
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))
	t.Setenv("TREEMAN_SOCKET", filepath.Join(t.TempDir(), "tm.sock"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// One repo, six committed files (each in its HEAD/placeholder form),
	// and a .treeman.yaml patching one key in each.
	files := map[string]string{
		".env":        "DB_HOST=127.0.0.1\nDB_DATABASE=placeholder\n",
		"phpunit.xml": "<?xml version=\"1.0\"?>\n<phpunit><php><env name=\"DB_DATABASE\" value=\"placeholder\"/></php></phpunit>\n",
		"config.yaml": "database:\n  name: placeholder\n  host: 127.0.0.1\n",
		"config.json": "{\n  \"database\": {\n    \"name\": \"placeholder\",\n    \"host\": \"127.0.0.1\"\n  }\n}\n",
		"config.toml": "[database]\nname = \"placeholder\"\nhost = \"127.0.0.1\"\n",
		"config.ini":  "[database]\nname=placeholder\nhost=127.0.0.1\n",
	}
	treemanYAML := `patches:
  - file: .env
    set:
      DB_DATABASE: app_{slug}
  - file: phpunit.xml
    format: phpunit
    set:
      DB_DATABASE: app_{slug}
  - file: config.yaml
    set:
      database.name: app_{slug}
  - file: config.json
    set:
      database.name: app_{slug}
  - file: config.toml
    set:
      database.name: app_{slug}
  - file: config.ini
    set:
      database.name: app_{slug}
`

	main := t.TempDir()
	mustGit(t, main, "init", "-q", "-b", "main")
	mustGit(t, main, "config", "user.email", "t@t")
	mustGit(t, main, "config", "user.name", "t")
	for name, body := range files {
		writeFile(t, main, name, body)
	}
	writeFile(t, main, ".treeman.yaml", treemanYAML)
	mustGit(t, main, "add", "-A")
	mustGit(t, main, "commit", "-q", "-m", "init")

	// Linked worktree — patches only apply off the main checkout, and
	// the clean filter no-ops on the main worktree.
	wt := filepath.Join(main, ".worktrees", "feat")
	mustGit(t, main, "worktree", "add", "-q", "-b", "feature/x", wt)

	cfg, err := resolve.LoadResolvedForWorktree(main, wt)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.Patches) != len(files) {
		t.Fatalf("expected %d patches, got %d", len(files), len(cfg.Patches))
	}

	// Apply each patch (per-worktree rewrite) + wire the real filter.
	tplCtx := template.FromSlug(slug.For(wt, ""))
	rel := make([]string, 0, len(cfg.Patches))
	for _, p := range cfg.Patches {
		if _, err := patcher.Apply(p, wt, tplCtx); err != nil {
			t.Fatalf("apply %s: %v", p.File, err)
		}
		rel = append(rel, p.File)
	}
	if err := patcher.EnsureFilter(ctx, wt, rel); err != nil {
		t.Fatalf("EnsureFilter: %v", err)
	}

	// Each file carries the per-worktree value on disk…
	for f := range files {
		body, err := os.ReadFile(filepath.Join(wt, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(body), "app_") {
			t.Errorf("%s was not patched on disk: %q", f, body)
		}
	}

	// …yet none of them show as modified: the clean filter projects the
	// patched key back to HEAD, so git sees the tracked file as unchanged.
	porcelain := gitStatusPorcelain(t, wt)
	for f := range files {
		if strings.Contains(porcelain, f) {
			t.Errorf("patched %s shows as a git modification — clean filter not byte-stable for this driver:\n%s", f, porcelain)
		}
	}
	if strings.TrimSpace(porcelain) != "" {
		t.Logf("worktree git status (context):\n%s", porcelain)
	}

	// The index stat cache must be refreshed too, not just consistent in
	// content. patcher.Apply rewrote each file, moving its size+mtime; if
	// EnsureFilter's `git add --renormalize` did not re-stage, every later
	// `git status` re-derives "modified" from the stale stat entry without
	// persisting a refresh — the file then looks dirty forever even though
	// index blob == HEAD blob (issue #29). `update-index --refresh` prints
	// "<file>: needs update" and exits non-zero in exactly that state.
	refresh := exec.Command("git", "-C", wt, "update-index", "--refresh")
	if out, err := refresh.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("index stat cache is stale after patch + EnsureFilter (err=%v):\n%s", err, out)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func mustProjectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd() // .../e2e/patches
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func gitStatusPorcelain(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status --porcelain: %v\n%s", err, out)
	}
	return string(out)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", full, err, out)
	}
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
