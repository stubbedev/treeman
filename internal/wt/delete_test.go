package wt

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteValidation(t *testing.T) {
	ctx := context.Background()
	_, err := Delete(ctx, DeleteRequest{}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "target is required") {
		t.Fatalf("missing target should error, got %v", err)
	}
	_, err = Delete(ctx, DeleteRequest{Target: "x"}, NoopSink{})
	if err == nil || !strings.Contains(err.Error(), "repo_root is required") {
		t.Fatalf("missing repo_root should error, got %v", err)
	}
}

func TestDeleteNoMatchWithoutForce(t *testing.T) {
	withTempStore(t)
	// Repo root that exists; target that doesn't match anything.
	repoRoot := t.TempDir()
	missingPath := filepath.Join(repoRoot, "does-not-exist")
	_, err := Delete(context.Background(), DeleteRequest{
		RepoRoot: repoRoot,
		Target:   missingPath,
	}, NoopSink{})
	if err == nil {
		t.Fatal("expected error for missing target without --force")
	}
	if !strings.Contains(err.Error(), "no worktree matches") {
		t.Errorf("error should mention 'no worktree matches', got: %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should hint at --force, got: %v", err)
	}
}
