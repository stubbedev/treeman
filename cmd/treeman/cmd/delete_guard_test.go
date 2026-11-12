package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteGuardFailsWhenGitCannotInspectTheWorktree(t *testing.T) {
	dir := t.TempDir()
	if reasons, err := unregenerableState(context.Background(), dir); err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("unregenerableState(non-repo) = %v, %v; want an error naming the path instead of \"nothing to lose\"", reasons, err)
	}
	if m := worktreeMarkers(context.Background(), dir); m != " ?" {
		t.Fatalf("worktreeMarkers(non-repo) = %q; want \" ?\" so an unreadable worktree is not shown as clean", m)
	}
}

func TestDeleteTargetRisksTreatsAMissingDirectoryAsClean(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "removed-worktree")
	reasons, err := deleteTargetRisks(context.Background(), gone)
	if err != nil || len(reasons) != 0 {
		t.Fatalf("deleteTargetRisks(missing dir) = %v, %v; want no reasons and no error", reasons, err)
	}
	present := t.TempDir()
	if reasons, err := deleteTargetRisks(context.Background(), present); err == nil || !strings.Contains(err.Error(), present) {
		t.Fatalf("deleteTargetRisks(non-repo dir) = %v, %v; want the probe error naming the path", reasons, err)
	}
}
