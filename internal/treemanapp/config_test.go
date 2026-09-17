package treemanapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/config"
)

func TestConfigFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"env", nil, "env"},
		{"flag", []string{"--config", "flag.yaml"}, "flag"},
		{"empty", []string{"--config="}, "env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("TREEMAN_CONFIG", "env.yaml")
			for _, name := range []string{"flag", "env"} {
				if err := os.WriteFile(name+".yaml", []byte("worktrees:\n  root: "+name+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			app := New()
			called := false
			app.Commands = []*cli.Command{{Name: "probe", Action: func(context.Context, *cli.Command) error {
				called = true
				t.Chdir(t.TempDir())
				cfg, err := config.LoadGlobal()
				if err != nil || cfg.Worktrees.Root != tc.want {
					t.Fatalf("config root = %q, error %v", cfg.Worktrees.Root, err)
				}
				return nil
			}}}
			args := append([]string{"treeman"}, tc.args...)
			args = append(args, "probe")
			if err := app.Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("action not called")
			}
			if path, _ := config.GlobalConfigPath(); path != "env.yaml" {
				t.Fatalf("invocation override leaked: %q", path)
			}
		})
	}
}

func TestConfigFlagMissing(t *testing.T) {
	t.Setenv("TREEMAN_CONFIG", "")
	path := filepath.Join(t.TempDir(), "missing.yaml")
	app := New()
	app.Commands = []*cli.Command{{Name: "probe", Action: func(context.Context, *cli.Command) error {
		t.Fatal("action called with missing config")
		return nil
	}}}
	if err := app.Run(
		context.Background(),
		[]string{"treeman", "--config", path, "probe"},
	); err == nil ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("missing config error: %v", err)
	}
}
