package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBringInFilesCopiesGitignoredFile(t *testing.T) {
	main := t.TempDir()
	wt := t.TempDir()
	src := filepath.Join(main, ".env")
	if err := os.WriteFile(src, []byte("DB_NAME=app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bringInFiles(main, wt, []string{".env"}, "copy"); err != nil {
		t.Fatalf("bringInFiles: %v", err)
	}
	dst := filepath.Join(wt, ".env")
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
	wt := t.TempDir()
	src := filepath.Join(main, "vendor")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bringInFiles(main, wt, []string{"vendor"}, "link"); err != nil {
		t.Fatalf("bringInFiles: %v", err)
	}
	dst := filepath.Join(wt, "vendor")
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
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(main, ".env"), []byte("X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bringInFiles(main, wt, []string{".env"}, "copy"); err != nil {
		t.Fatal(err)
	}
	// User mutates the worktree's copy.
	if err := os.WriteFile(filepath.Join(wt, ".env"), []byte("X=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Re-run: should NOT overwrite the mutated copy.
	if err := bringInFiles(main, wt, []string{".env"}, "copy"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(wt, ".env"))
	if string(got) != "X=2\n" {
		t.Errorf("idempotent re-run overwrote user edit: %q", got)
	}
}

func TestBringInFilesCopiesDirectoryRecursively(t *testing.T) {
	main := t.TempDir()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, "seeds", "fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "a.sql"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "seeds", "fixtures", "b.sql"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bringInFiles(main, wt, []string{"seeds"}, "copy"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "seeds", "a.sql")); string(got) != "a" {
		t.Errorf("missing seeds/a.sql in copy: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "seeds", "fixtures", "b.sql")); string(got) != "b" {
		t.Errorf("missing seeds/fixtures/b.sql in copy: %q", got)
	}
}
