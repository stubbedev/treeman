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
