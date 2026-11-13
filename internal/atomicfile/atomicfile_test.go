package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".env")
	if err := os.Symlink("real.env", link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced by a regular file: %v", err)
	}
	got, _ := os.ReadFile(target)
	fi, _ := os.Stat(target)
	if string(got) != "new" || fi.Mode().Perm() != 0o640 {
		t.Errorf("target = %q mode %v, want \"new\" mode 0640", got, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestWriteNewFileUsesPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := Write(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestWriteRefusesDanglingSymlink(t *testing.T) {
	link := filepath.Join(t.TempDir(), "x")
	if err := os.Symlink("missing", link); err != nil {
		t.Fatal(err)
	}
	if err := Write(link, []byte("x"), 0o600); err == nil {
		t.Fatal("dangling symlink must be refused")
	}
}

func TestRefreshMovesToFreshInodeKeepingContentModeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "phpunit.xml")
	if err := os.WriteFile(path, []byte("<php/>\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2001, 2, 3, 4, 5, 6, 789, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Refresh(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("file kept its inode")
	}
	if got, _ := os.ReadFile(path); string(got) != "<php/>\n" {
		t.Errorf("content = %q", got)
	}
	if after.Mode().Perm() != 0o640 || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mode %v mtime %v, want 0640 %v", after.Mode().Perm(), after.ModTime(), before.ModTime())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestRefreshMissingFileFails(t *testing.T) {
	if err := Refresh(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file must be an error")
	}
}
