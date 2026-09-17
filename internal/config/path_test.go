package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePathConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalConfigSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		flag string
		want uint64
	}{
		{name: "default", want: 101},
		{name: "env", env: "env.yaml", want: 202},
		{name: "flag", flag: "flag.yaml", want: 303},
		{name: "flag over env", env: "missing.yaml", flag: "flag.yaml", want: 303},
		{name: "empty flag uses env", env: "env.yaml", flag: "", want: 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("TREEMAN_CONFIG", tc.env)
			writePathConfig(t, filepath.Join(root, "treeman", "config.yaml"), "debounce_ms: 101\nlogs:\n  keep_days: 99\n")
			writePathConfig(t, "env.yaml", "debounce_ms: 202\n")
			writePathConfig(t, "flag.yaml", "debounce_ms: 303\n")
			restore, err := ConfigureGlobalPath(tc.flag)
			if err != nil {
				t.Fatal(err)
			}
			defer restore()
			if path, ok := GlobalConfigPath(); !ok || !filepath.IsAbs(path) {
				t.Fatalf("config path = %q, %v", path, ok)
			}
			t.Chdir(t.TempDir())
			cfg, err := LoadGlobal()
			if err != nil || cfg.DebounceMs != tc.want {
				t.Fatalf("debounce = %d, want %d; error %v", cfg.DebounceMs, tc.want, err)
			}
			if tc.want != 101 && cfg.Logs.KeepDays != 14 {
				t.Fatalf("default global leaked into override: %+v", cfg.Logs)
			}
			if os.Getenv("TREEMAN_CONFIG") != tc.env {
				t.Fatal("selection changed environment")
			}
		})
	}
}

func TestConfigOverrideLoaders(t *testing.T) {
	for _, loader := range []struct {
		name string
		load func(string, string) (Config, error)
	}{
		{"global", func(_, _ string) (Config, error) { return LoadGlobal() }},
		{"layered", func(repo, _ string) (Config, error) { return LoadLayered(repo) }},
		{"worktree", LoadLayeredForWorktree},
	} {
		t.Run(loader.name, func(t *testing.T) {
			root, repo, wt := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			t.Setenv("TREEMAN_CONFIG", "")
			cfg, err := loader.load(repo, wt)
			if err != nil || cfg.DebounceMs != 500 {
				t.Fatalf("missing default: %+v, %v", cfg, err)
			}
			path := filepath.Join(root, "alternate.yaml")
			t.Setenv("TREEMAN_CONFIG", path)
			if _, err := loader.load(repo, wt); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("missing explicit: %v", err)
			}
			writePathConfig(t, path, "")
			if cfg, err := loader.load(repo, wt); err != nil || cfg.DebounceMs != 500 {
				t.Fatalf("empty explicit: %+v, %v", cfg, err)
			}
			writePathConfig(t, path, "hooks: {}\n")
			if _, err := loader.load(repo, wt); err == nil || !strings.Contains(err.Error(), "belongs in the repo config") {
				t.Fatalf("global scope: %v", err)
			}
			writePathConfig(t, path, "debounce_ms: 101\n")
			if cfg, err := loader.load(repo, wt); err != nil || cfg.DebounceMs != 101 {
				t.Fatalf("override: %+v, %v", cfg, err)
			}
			if loader.name == "global" {
				return
			}
			checkOverridePrecedence(t, loader.load, loader.name == "worktree", repo, wt)
			writePathConfig(t, filepath.Join(repo, ".treeman.yaml"), "daemon:\n  log_level: debug\n")
			if _, err := loader.load(repo, wt); err == nil || !strings.Contains(err.Error(), "belongs in the global config") {
				t.Fatalf("repo scope: %v", err)
			}
		})
	}
}

func checkOverridePrecedence(t *testing.T, load func(string, string) (Config, error), worktree bool, repo, wt string) {
	t.Helper()
	files := []string{filepath.Join(repo, ".treeman.yaml"), filepath.Join(repo, ".treeman.local.yaml")}
	if worktree {
		files = append(files, filepath.Join(wt, ".treeman.local.yaml"))
	}
	for i, file := range files {
		writePathConfig(t, file, "debounce_ms: "+[]string{"202", "303", "404"}[i]+"\n")
		if cfg, err := load(repo, wt); err != nil || cfg.DebounceMs != uint64((i+2)*101) {
			t.Fatalf("precedence %s: %d, %v", file, cfg.DebounceMs, err)
		}
	}
}

func TestConfigureGlobalPathMissing(t *testing.T) {
	t.Setenv("TREEMAN_CONFIG", "")
	path := filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := ConfigureGlobalPath(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing flag: %v", err)
	}
	if explicitGlobalConfigPath() != "" {
		t.Fatal("failed selection leaked")
	}
}

func TestGlobalConfigHomeDefault(t *testing.T) {
	t.Setenv("TREEMAN_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".config", "treeman", "config.yaml")
	writePathConfig(t, path, "debounce_ms: 123\n")
	if got, ok := GlobalConfigPath(); !ok || got != path {
		t.Fatalf("default path = %q, %v", got, ok)
	}
	if cfg, err := LoadGlobal(); err != nil || cfg.DebounceMs != 123 {
		t.Fatalf("home default: %d, %v", cfg.DebounceMs, err)
	}
}
