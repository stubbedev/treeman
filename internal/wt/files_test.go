package wt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBringInFilesCopiesGitignoredFile(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	src := filepath.Join(main, ".env")
	if err := os.WriteFile(src, []byte("DB_NAME=app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles: %v", err)
	}
	dst := filepath.Join(wtDir, ".env")
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("dst is a symlink, want a real file")
	}
	// Mutate the worktree's copy; main's copy must be unaffected.
	if err := os.WriteFile(dst, []byte("DB_NAME=app_feature-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainBody, _ := os.ReadFile(src)
	if string(mainBody) != "DB_NAME=app\n" {
		t.Errorf("main mutated by worktree edit: %q", mainBody)
	}
}

func TestBringInFilesSymlinkMode(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	src := filepath.Join(main, "vendor")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(main, wtDir, []string{"vendor"}, "link", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles: %v", err)
	}
	dst := filepath.Join(wtDir, "vendor")
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dst is not a symlink")
	}
	target, _ := os.Readlink(dst)
	if target != src {
		t.Errorf("symlink target = %q, want %q", target, src)
	}
}

func TestBringInFilesIdempotent(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
		t.Fatal(err)
	}
	// User mutates the worktree's copy.
	if err := os.WriteFile(filepath.Join(wtDir, ".env"), []byte("X=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-run: should NOT overwrite the mutated copy.
	if err := BringInFiles(main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(wtDir, ".env"))
	if string(got) != "X=2\n" {
		t.Errorf("idempotent re-run overwrote user edit: %q", got)
	}
}

// TestBringInFilesDoublestarGlobMatchesBaseAndNested guards the same
// `**`-silent-drop bug class fixed in the snapshot fingerprint: a
// `config/**/*.local.php` copy glob must bring in files sitting
// directly in the base dir AND nested deeper. stdlib filepath.Glob
// would silently drop the base-dir file.
func TestBringInFilesDoublestarGlobMatchesBaseAndNested(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	base := filepath.Join(main, "config", "app.local.php")
	nested := filepath.Join(main, "config", "modules", "mod.local.php")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("nested"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(main, wtDir, []string{"config/**/*.local.php"}, "copy", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "config", "app.local.php")); string(got) != "base" {
		t.Errorf("base-dir file not brought in (stdlib ** drop bug): %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "config", "modules", "mod.local.php")); string(got) != "nested" {
		t.Errorf("nested file not brought in: %q", got)
	}
}

// TestBringInFilesBraceGlob guards brace alternation (`{a,b}`), which
// doublestar supports but stdlib filepath.Glob does not — and which
// the old `*?[` meta check would have treated as a literal path.
func TestBringInFilesBraceGlob(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	for _, n := range []string{".env", ".env.local"} {
		if err := os.WriteFile(filepath.Join(main, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := BringInFiles(main, wtDir, []string{"{.env,.env.local}"}, "copy", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles: %v", err)
	}
	for _, n := range []string{".env", ".env.local"} {
		if got, _ := os.ReadFile(filepath.Join(wtDir, n)); string(got) != n {
			t.Errorf("brace glob did not bring in %s: %q", n, got)
		}
	}
}

func TestBringInFilesCopiesDirectoryRecursively(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, "seeds", "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "a.sql"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "fixtures", "b.sql"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(main, wtDir, []string{"seeds"}, "copy", NoopSink{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "seeds", "a.sql")); string(got) != "a" {
		t.Errorf("missing seeds/a.sql in copy: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "seeds", "fixtures", "b.sql")); string(got) != "b" {
		t.Errorf("missing seeds/fixtures/b.sql in copy: %q", got)
	}
}

// TestBringInFilesReportCountsAndSkips guards the per-entry observability
// report: files + bytes are tallied on first copy, and a second
// idempotent pass reports the entry as skipped with zero files copied.
func TestBringInFilesReportCountsAndSkips(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, "seeds", "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "a.sql"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "fixtures", "b.sql"), []byte("bb"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := BringInFilesReport(main, wtDir, []string{"seeds"}, "copy", NoopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d", len(res))
	}
	if res[0].Files != 2 || res[0].Bytes != 5 {
		t.Errorf("first pass: files/bytes = %d/%d, want 2/5", res[0].Files, res[0].Bytes)
	}
	if res[0].Brought != 1 || res[0].Skipped != 0 {
		t.Errorf("first pass: brought/skipped = %d/%d, want 1/0", res[0].Brought, res[0].Skipped)
	}

	// Idempotent second pass: dst exists → skipped, nothing copied.
	res2, err := BringInFilesReport(main, wtDir, []string{"seeds"}, "copy", NoopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if res2[0].Brought != 0 || res2[0].Skipped != 1 || res2[0].Files != 0 {
		t.Errorf("second pass: brought/skipped/files = %d/%d/%d, want 0/1/0",
			res2[0].Brought, res2[0].Skipped, res2[0].Files)
	}

	// Missing non-glob source is tallied, not fatal.
	res3, err := BringInFilesReport(main, wtDir, []string{"nope"}, "copy", NoopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if res3[0].Missing != 1 || res3[0].Brought != 0 {
		t.Errorf("missing source: missing/brought = %d/%d, want 1/0", res3[0].Missing, res3[0].Brought)
	}
}

func TestRemoveWorktreeTree(t *testing.T) {
	wtRoot := t.TempDir()

	// Leftover under wtRoot (the post-`git worktree remove` node_modules
	// case): the worktree dir and its now-empty feature/ parent both go.
	wtPath := filepath.Join(wtRoot, "feature", "KON-1")
	if err := os.MkdirAll(filepath.Join(wtPath, "frontend", "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	RemoveWorktreeTree(wtPath, wtRoot)
	if _, err := os.Stat(filepath.Join(wtRoot, "feature")); !os.IsNotExist(err) {
		t.Errorf("leftover/empty parent not removed: stat err = %v", err)
	}
	if _, err := os.Stat(wtRoot); err != nil {
		t.Errorf("wtRoot wrongly removed by parent walk: %v", err)
	}

	// A non-empty parent stops the walk (sibling worktree must survive).
	keep := filepath.Join(wtRoot, "feature", "KON-2")
	gone := filepath.Join(wtRoot, "feature", "KON-3")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	RemoveWorktreeTree(gone, wtRoot)
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("sibling worktree removed: %v", err)
	}

	// Guard: wtRoot itself, outside paths, and empty args are no-ops.
	outside := t.TempDir()
	RemoveWorktreeTree(wtRoot, wtRoot)
	RemoveWorktreeTree(outside, wtRoot)
	RemoveWorktreeTree("", wtRoot)
	RemoveWorktreeTree(wtRoot, "")
	for _, p := range []string{wtRoot, outside} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("guarded path %q wrongly removed: %v", p, err)
		}
	}
}
