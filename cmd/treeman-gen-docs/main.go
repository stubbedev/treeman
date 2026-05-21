// Command treeman-gen-docs walks the urfave/cli v3 command tree
// declared by treeman.App() and emits a markdown CLI reference to
// `docs/cli.md`. Wired into `just sync-docs` so PRs catch doc drift.
//
// The output is intentionally human-curated-looking (collapsible
// `<details>` per command, code-fenced flag tables) so the file
// remains readable when viewed on GitHub.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/treemanapp"
)

func main() {
	app := treemanapp.New()
	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString("[← back to README](../README.md)\n\n")
	b.WriteString("Auto-generated from the `treeman` binary's command tree.\n")
	b.WriteString("Run `just sync-docs` after touching any subcommand to refresh.\n\n")
	b.WriteString("## Commands\n\n")

	for _, sub := range app.Commands {
		if sub.Hidden {
			continue
		}
		renderCommand(&b, sub, []string{app.Name})
	}

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}

func renderCommand(b *strings.Builder, c *cli.Command, parents []string) {
	full := append(parents, c.Name)
	path := strings.Join(full, " ")

	fmt.Fprintf(b, "### `%s`\n\n", path)
	if len(c.Aliases) > 0 {
		fmt.Fprintf(b, "Aliases: %s\n\n", "`"+strings.Join(c.Aliases, "`, `")+"`")
	}
	if c.Usage != "" {
		fmt.Fprintf(b, "%s\n\n", c.Usage)
	}
	if c.Description != "" {
		fmt.Fprintf(b, "```\n%s\n```\n\n", strings.TrimSpace(c.Description))
	}

	flags := visibleFlags(c)
	if len(flags) > 0 {
		b.WriteString("| Flag | Usage |\n|---|---|\n")
		for _, f := range flags {
			names := f.Names()
			sort.Slice(names, func(i, j int) bool { return len(names[i]) < len(names[j]) })
			label := ""
			for i, n := range names {
				if i > 0 {
					label += ", "
				}
				if len(n) == 1 {
					label += "`-" + n + "`"
				} else {
					label += "`--" + n + "`"
				}
			}
			usage := flagUsage(f)
			fmt.Fprintf(b, "| %s | %s |\n", label, escapePipes(usage))
		}
		b.WriteString("\n")
	}

	for _, sub := range c.Commands {
		if sub.Hidden {
			continue
		}
		renderCommand(b, sub, full)
	}
}

// visibleFlags returns user-facing flags only (drops `--help`).
func visibleFlags(c *cli.Command) []cli.Flag {
	var out []cli.Flag
	for _, f := range c.Flags {
		names := f.Names()
		if len(names) > 0 && (names[0] == "help" || names[0] == "h") {
			continue
		}
		out = append(out, f)
	}
	return out
}

// flagUsage extracts the user-facing usage string from any cli.Flag
// implementation by reflecting on the embedded `Usage` field. urfave
// flags all expose this through their public struct fields.
func flagUsage(f cli.Flag) string {
	// All cli.Flag impls satisfy the docGenerationFlag interface
	// (GetUsage) — use that when available.
	type docFlag interface{ GetUsage() string }
	if df, ok := f.(docFlag); ok {
		return df.GetUsage()
	}
	return f.String()
}

func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
