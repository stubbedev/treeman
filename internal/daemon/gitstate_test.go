package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGitOpInProgress covers each conflict-prone marker (files and the
// rebase directories) plus the clean case.
func TestGitOpInProgress(t *testing.T) {
	cases := []struct {
		marker string
		isDir  bool
		want   string
	}{
		{"MERGE_HEAD", false, "merge"},
		{"rebase-merge", true, "rebase"},
		{"rebase-apply", true, "rebase"},
		{"CHERRY_PICK_HEAD", false, "cherry-pick"},
		{"REVERT_HEAD", false, "revert"},
	}
	for _, c := range cases {
		t.Run(c.marker, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, c.marker)
			if c.isDir {
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitOpInProgress(dir); got != c.want {
				t.Errorf("gitOpInProgress = %q, want %q", got, c.want)
			}
		})
	}

	t.Run("clean", func(t *testing.T) {
		if got := gitOpInProgress(t.TempDir()); got != "" {
			t.Errorf("gitOpInProgress(clean) = %q, want \"\"", got)
		}
	})
}

// TestIsGitOpMarker guards the watcher's event-filter widening.
func TestIsGitOpMarker(t *testing.T) {
	for _, base := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if !isGitOpMarker(base) {
			t.Errorf("isGitOpMarker(%q) = false, want true", base)
		}
	}
	for _, base := range []string{"HEAD", "ORIG_HEAD", "index", "config"} {
		if isGitOpMarker(base) {
			t.Errorf("isGitOpMarker(%q) = true, want false", base)
		}
	}
}
