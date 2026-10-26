package treemanapp

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// strayArgsTree is a minimal stand-in for the real tree: one leaf that
// takes no arguments (with its own Before), one that declares them, one
// that parses its own argv, and a group.
func strayArgsTree(ran map[string][]string, beforeRan *bool) *cli.Command {
	record := func(name string) cli.ActionFunc {
		return func(_ context.Context, c *cli.Command) error {
			ran[name] = c.Args().Slice()
			return nil
		}
	}
	root := &cli.Command{
		Name:                  "treeman",
		EnableShellCompletion: true,
		Writer:                io.Discard,
		ErrWriter:             io.Discard,
		Commands: []*cli.Command{
			{
				Name:   "prepare",
				Action: record("prepare"),
				Before: func(ctx context.Context, _ *cli.Command) (context.Context, error) {
					*beforeRan = true
					return ctx, nil
				},
			},
			{Name: "slug", ArgsUsage: "[path]", Action: record("slug")},
			{Name: "mcp", SkipFlagParsing: true, Action: record("mcp")},
			{Name: "daemon", Commands: []*cli.Command{
				{Name: "status", Action: record("daemon status")},
			}},
		},
	}
	rejectStrayArgs(root)
	return root
}

func TestRejectStrayArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string // substring; empty = success
		wantRan string // action expected to have run
	}{
		{
			name: "no-arg leaf rejects a stray argument", args: []string{"prepare", "status"},
			wantErr: "unexpected argument \"status\": `treeman prepare` takes no positional arguments",
		},
		{name: "no-arg leaf still runs bare", args: []string{"prepare"}, wantRan: "prepare"},
		{
			name: "nested no-arg leaf rejects", args: []string{"daemon", "status", "xyz"},
			wantErr: "`treeman daemon status` takes no positional arguments",
		},
		{name: "declared arguments pass through", args: []string{"slug", "/some/path"}, wantRan: "slug"},
		{name: "SkipFlagParsing command keeps its argv", args: []string{"mcp", "--http", ":8080"}, wantRan: "mcp"},
		{name: "shell completion is unaffected", args: []string{"prepare", "--generate-shell-completion"}},
		{name: "help is unaffected", args: []string{"prepare", "--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := map[string][]string{}
			beforeRan := false
			err := strayArgsTree(ran, &beforeRan).Run(context.Background(), append([]string{"treeman"}, tc.args...))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(ran) != 0 {
					t.Fatalf("an action ran despite the stray argument: %v", ran)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantRan != "" {
				if _, ok := ran[tc.wantRan]; !ok {
					t.Fatalf("%q did not run (ran: %v)", tc.wantRan, ran)
				}
			}
			if tc.wantRan == "prepare" && !beforeRan {
				t.Fatal("the command's own Before was dropped")
			}
		})
	}
}

// TestEveryLeafGuardedOrDeclared walks the real tree: a leaf either
// declares its positional arguments or carries the stray-argument
// guard. The commands that read arguments must keep declaring them, or
// the guard would start rejecting their real input.
func TestEveryLeafGuardedOrDeclared(t *testing.T) {
	takesArgs := map[string]bool{
		"treeman worktree register":   true,
		"treeman worktree unregister": true,
		"treeman hook run":            true,
		"treeman git log":             true,
		"treeman slug":                true,
	}
	var walk func(c *cli.Command, path string)
	walk = func(c *cli.Command, path string) {
		if len(c.Commands) > 0 {
			for _, sub := range c.Commands {
				walk(sub, path+" "+sub.Name)
			}
			return
		}
		declared := c.ArgsUsage != "" || len(c.Arguments) > 0 || c.SkipFlagParsing
		if takesArgs[path] && !declared {
			t.Errorf("%s reads positional arguments but declares none", path)
		}
		if c.Action != nil && !declared && c.Before == nil {
			t.Errorf("%s declares no positional arguments but has no stray-argument guard", path)
		}
	}
	walk(New(), "treeman")
}
