// Command treeman is the CLI for the treeman per-worktree DB
// orchestrator. The subcommand tree is treated as a stable user-
// facing contract so existing scripts (gwt zsh integration, CI
// flows) keep working across releases.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/cmd/treeman/cmd"
	"github.com/stubbedev/treeman/internal/treemanapp"
)

func main() {
	app := treemanapp.New()
	wireCommandNotFound(app)
	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "treeman:", err)
		os.Exit(1)
	}
}

// wireCommandNotFound installs a CommandNotFound handler on `c` and
// every nested subcommand recursively. The handler prints a short
// "unknown command" line plus up-to-3 nearest matches by Levenshtein
// distance, then exits 1.
func wireCommandNotFound(c *cli.Command) {
	c.CommandNotFound = func(ctx context.Context, parent *cli.Command, typed string) {
		names := make([]string, 0, len(parent.Commands)*2)
		for _, sub := range parent.Commands {
			names = append(names, sub.Name)
			names = append(names, sub.Aliases...)
		}
		path := "treeman"
		helpPath := "--help"
		if parent.Name != "treeman" {
			path = "treeman " + parent.Name
			helpPath = parent.Name + " --help"
		}
		fmt.Fprintf(os.Stderr, "treeman: unknown command %q for `%s`\n", typed, path)
		if hints := cmd.SuggestNearestCommands(typed, names, 3); len(hints) > 0 {
			fmt.Fprintf(os.Stderr, "  did you mean: %s ?\n", strings.Join(hints, ", "))
		}
		fmt.Fprintf(os.Stderr, "  run `treeman %s` for available commands\n", helpPath)
		os.Exit(1)
	}
	for _, sub := range c.Commands {
		wireCommandNotFound(sub)
	}
}
