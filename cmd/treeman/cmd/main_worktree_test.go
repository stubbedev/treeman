package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stubbedev/treeman/internal/config"
)

// TestPatchMainWorktreeConfigVirginRepo confirms that running
// `treeman main enable` against a repo with no .treeman.yaml writes
// a YAML body that re-parses cleanly into config.Config. Without
// this guard a virgin-repo enable could land an empty or malformed
// document on disk.
func TestPatchMainWorktreeConfigVirginRepo(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))

	if err := patchMainWorktreeConfig(context.Background(), repo, true); err != nil {
		t.Fatalf("patch: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("written file is empty")
	}
	var cfg config.Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("written body does not parse as config.Config: %v\nbody=%q", err, body)
	}
	if !cfg.MainWorktree.Enabled {
		t.Errorf("expected main_worktree.enabled=true, got %+v\nbody=%q",
			cfg.MainWorktree, body)
	}
}

// TestPatchMainWorktreeConfigPreservesExistingKeys verifies that the
// surgical YAML edit doesn't trample sibling keys in an existing
// .treeman.yaml.
func TestPatchMainWorktreeConfigPreservesExistingKeys(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("TREEMAN_DB_PATH", filepath.Join(t.TempDir(), "treeman.db"))
	original := "auto_fetch:\n  enabled: false\n  interval_minutes: 5\n"
	if err := os.WriteFile(filepath.Join(repo, ".treeman.yaml"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := patchMainWorktreeConfig(context.Background(), repo, true); err != nil {
		t.Fatalf("patch: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(repo, ".treeman.yaml"))
	var cfg config.Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("parse: %v\n%s", err, body)
	}
	if cfg.AutoFetch.IsEnabled() {
		t.Errorf("auto_fetch.enabled trampled; got %+v\n%s", cfg.AutoFetch, body)
	}
	if cfg.AutoFetch.IntervalMinutes != 5 {
		t.Errorf("auto_fetch.interval_minutes trampled; got %d\n%s",
			cfg.AutoFetch.IntervalMinutes, body)
	}
	if !cfg.MainWorktree.Enabled {
		t.Errorf("main_worktree.enabled not set; body=%s", body)
	}
}
