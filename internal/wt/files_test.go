package wt

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBringInFilesCopiesGitignoredFile(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	src := filepath.Join(main, ".env")
	if err := os.WriteFile(src, []byte("DB_NAME=app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := BringInFiles(context.Background(), main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
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
	if err := BringInFiles(context.Background(), main, wtDir, []string{"vendor"}, "link", NoopSink{}); err != nil {
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
	if err := BringInFiles(context.Background(), main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
		t.Fatal(err)
	}
	// User mutates the worktree's copy.
	if err := os.WriteFile(filepath.Join(wtDir, ".env"), []byte("X=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-run: should NOT overwrite the mutated copy.
	if err := BringInFiles(context.Background(), main, wtDir, []string{".env"}, "copy", NoopSink{}); err != nil {
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
	if err := BringInFiles(context.Background(), main, wtDir, []string{"config/**/*.local.php"}, "copy", NoopSink{}); err != nil {
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
	if err := BringInFiles(context.Background(), main, wtDir, []string{"{.env,.env.local}"}, "copy", NoopSink{}); err != nil {
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
	if err := BringInFiles(context.Background(), main, wtDir, []string{"seeds"}, "copy", NoopSink{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "seeds", "a.sql")); string(got) != "a" {
		t.Errorf("missing seeds/a.sql in copy: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wtDir, "seeds", "fixtures", "b.sql")); string(got) != "b" {
		t.Errorf("missing seeds/fixtures/b.sql in copy: %q", got)
	}
}

// TestBringInFilesCopiesLargeTreeConcurrently stresses the fan-out copy
// path (copyFanout workers + reflink-or-stream per file) on a tree far
// wider than the worker pool: every file must arrive with its content
// and mode intact, and the report's file/byte tallies must be exact.
func TestBringInFilesCopiesLargeTreeConcurrently(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	const dirs, perDir = 8, 25
	var wantBytes int64
	for d := range dirs {
		dir := filepath.Join(main, "vendor", "pkg"+strconv.Itoa(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := range perDir {
			body := strings.Repeat("x", d*perDir+f+1)
			wantBytes += int64(len(body))
			perm := os.FileMode(0o644)
			if f%5 == 0 {
				perm = 0o755
			}
			if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(f)+".js"), []byte(body), perm); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := BringInFilesReport(context.Background(), main, wtDir, []string{"vendor"}, "copy", NoopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Files != dirs*perDir || res[0].Bytes != wantBytes {
		t.Fatalf("report mismatch: %+v (want files=%d bytes=%d)", res, dirs*perDir, wantBytes)
	}
	for d := range dirs {
		for f := range perDir {
			path := filepath.Join(wtDir, "vendor", "pkg"+strconv.Itoa(d), "f"+strconv.Itoa(f)+".js")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing copy %s: %v", path, err)
			}
			if want := d*perDir + f + 1; len(got) != want {
				t.Fatalf("%s: got %d bytes, want %d", path, len(got), want)
			}
			info, _ := os.Stat(path)
			wantPerm := os.FileMode(0o644)
			if f%5 == 0 {
				wantPerm = 0o755
			}
			if info.Mode().Perm() != wantPerm {
				t.Fatalf("%s: mode %v, want %v", path, info.Mode().Perm(), wantPerm)
			}
		}
	}
}

// TestBringInFilesCancelledStopsBeforeNextEntry guards the cancellation
// contract relied on by daemon teardown: an already-cancelled ctx aborts
// the bring-in before the first entry is copied, returning ctx.Err() and
// leaving the destination untouched. Without this, a teardown that
// preempts an in-flight create finalize would let the copy run to
// completion and resurrect a dir the teardown just removed.
func TestBringInFilesCancelledStopsBeforeNextEntry(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, "seeds"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "a.sql"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := BringInFiles(ctx, main, wtDir, []string{"seeds"}, "copy", NoopSink{})
	if err == nil {
		t.Fatal("want error from cancelled ctx, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(wtDir, "seeds")); statErr == nil {
		t.Fatal("cancelled bring-in copied entry anyway")
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

	res, err := BringInFilesReport(context.Background(), main, wtDir, []string{"seeds"}, "copy", NoopSink{})
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
	res2, err := BringInFilesReport(context.Background(), main, wtDir, []string{"seeds"}, "copy", NoopSink{})
	if err != nil {
		t.Fatal(err)
	}
	if res2[0].Brought != 0 || res2[0].Skipped != 1 || res2[0].Files != 0 {
		t.Errorf("second pass: brought/skipped/files = %d/%d/%d, want 0/1/0",
			res2[0].Brought, res2[0].Skipped, res2[0].Files)
	}

	// Missing non-glob source is tallied, not fatal.
	res3, err := BringInFilesReport(context.Background(), main, wtDir, []string{"nope"}, "copy", NoopSink{})
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

// TestBringInFilesWritesGitExclude verifies brought-in paths are anchored
// into the repo's shared exclude so the worktree never reads as dirty, and
// that re-running neither duplicates a pattern nor re-adds the header.
func TestBringInFilesWritesGitExclude(t *testing.T) {
	main := t.TempDir()
	wtDir := t.TempDir()
	// gitCommonDir only needs <main>/.git to be a directory.
	if err := os.MkdirAll(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(main, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := BringInFiles(context.Background(), main, wtDir, []string{".claude"}, "link", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles: %v", err)
	}

	excludePath := filepath.Join(main, ".git", "info", "exclude")
	body, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(body), "\n/.claude\n") && !strings.HasSuffix(string(body), "/.claude\n") {
		t.Errorf("exclude missing anchored /.claude pattern:\n%s", body)
	}

	// Re-run: idempotent — no duplicate pattern, single header.
	if err := BringInFiles(context.Background(), main, wtDir, []string{".claude"}, "link", NoopSink{}); err != nil {
		t.Fatalf("BringInFiles re-run: %v", err)
	}
	body2, _ := os.ReadFile(excludePath)
	if got := strings.Count(string(body2), "/.claude\n"); got != 1 {
		t.Errorf("pattern count = %d, want 1:\n%s", got, body2)
	}
	if got := strings.Count(string(body2), excludeHeader); got != 1 {
		t.Errorf("header count = %d, want 1", got)
	}
}

func TestAnchoredPatterns(t *testing.T) {
	got := anchoredPatterns([]string{"./.claude", ".claude", "justfile", "", ".", "../escape", "a/b"})
	want := []string{"/.claude", "/justfile", "/a/b"}
	if len(got) != len(want) {
		t.Fatalf("anchoredPatterns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("anchoredPatterns[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
