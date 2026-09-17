package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stubbedev/treeman/internal/config"
)

func TestResolvedConfigOverrideCache(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "repo", true: "worktree"}[worktree], func(t *testing.T) {
			InvalidateConfigCache()
			t.Cleanup(InvalidateConfigCache)
			dir, repo, wt := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", dir)
			t.Setenv("TREEMAN_CONFIG", "")
			load := func() (config.Config, error) {
				if worktree {
					return LoadResolvedForWorktree(repo, wt)
				}
				return LoadResolved(repo)
			}
			if _, err := load(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "override.yaml")
			t.Setenv("TREEMAN_CONFIG", path)
			if _, err := load(); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("cache concealed missing override: %v", err)
			}
			for i, body := range []string{"debounce_ms: 111\n", "debounce_ms: 222\n"} {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Unix(int64(i+1), 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				if cfg, err := load(); err != nil || cfg.DebounceMs != uint64((i+1)*111) {
					t.Fatalf("override update: %d, %v", cfg.DebounceMs, err)
				}
			}
			t.Setenv("TREEMAN_CONFIG", "")
			if cfg, err := load(); err != nil || cfg.DebounceMs != 500 {
				t.Fatalf("override leaked: %d, %v", cfg.DebounceMs, err)
			}
		})
	}
}
