package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestAbsPathFailsWhenTheWorkingDirectoryIsGone(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	got, err := absPath("some-worktree")
	if err == nil {
		t.Fatalf("absPath with a deleted cwd = %q, nil; want an error instead of the unresolved path", got)
	}
	if !strings.Contains(err.Error(), "some-worktree") {
		t.Errorf("absPath error must name the input: %v", err)
	}
}
