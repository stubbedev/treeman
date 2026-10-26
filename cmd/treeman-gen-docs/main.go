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

// shellIntegrationSection is appended to docs/cli.md so the
// generated reference also documents completions and the cd shims.
var shellIntegrationSection = strings.Join([]string{
	"",
	"## Shell integration",
	"",
	"### Completions",
	"",
	"urfave/cli ships a `completion` generator; treeman keeps it visible and",
	"its per-command `ShellComplete` hooks fill live worktree names into",
	"`worktree go`, `worktree delete`, and `worktree show` arguments:",
	"",
	"    source <(treeman completion zsh)     # zsh",
	"    source <(treeman completion bash)    # bash",
	"    treeman completion fish | source     # fish",
	"    treeman completion pwsh | Out-String | Invoke-Expression   # pwsh",
	"",
	"### cd helpers",
	"",
	"`worktree go` and `worktree back` print resolved paths on stdout, so a",
	"shell function can `cd` for you. Ready-made shims per shell:",
	"",
	"| Shell | File | Install |",
	"|---|---|---|",
	"| zsh | `contrib/tm.zsh` | `source /path/to/treeman/contrib/tm.zsh` in `~/.zshrc` |",
	"| bash | `contrib/tm.bash` | `source /path/to/treeman/contrib/tm.bash` in `~/.bashrc` |",
	"| fish | `contrib/tm.fish` | `source /path/to/treeman/contrib/tm.fish` in `config.fish` |",
	"",
	"All three expose the same `tm` usage matrix:",
	"",
	"    tm proj-123          # cd into existing worktree (or report missing)",
	"    tm proj-123 -c       # create + cd to new worktree",
	"    tm new proj-123      # same as `tm proj-123 -c`",
	"    tm -                 # cd back to main repo",
	"    tm - --remove        # cd back + drop current worktree if clean",
	"    tm list              # passthrough to `treeman worktree list`",
	"",
	"## Exit codes",
	"",
	"Scripts can rely on the following process exit codes:",
	"",
	"| Code | Meaning |",
	"|---|---|",
	"| 0 | Command succeeded — including informational results like `daemon status` reporting the daemon down (a status, not a failure) |",
	"| 1 | Command failed: bad arguments, unreachable engine, denied confirmation, any hard error |",
	"| 3 | Daemon unavailable — only from `treeman daemon status --fail-if-down` (or `--json` with the flag): a CI gate signal distinct from 1, so \"daemon down\" never reads as \"command broken\" |",
	"",
	"`--json` output shape is part of the same contract: a failing command's\nJSON (when any) lands on stdout, human diagnostics on stderr. `daemon\nstatus --json` always emits a JSON object with a `status` field of\n`running`, `not-running`, or `protocol-mismatch`.",
	"",
	"",
}, "\n")

func main() {
	app := treemanapp.New()
	var b strings.Builder
	b.WriteString("# CLI reference\n\n")
	b.WriteString("Auto-generated from the `treeman` binary's command tree.\n")
	b.WriteString("Run `just sync-docs` after touching any subcommand to refresh.\n\n")
	b.WriteString("## Commands\n\n")

	for _, sub := range app.Commands {
		if sub.Hidden {
			continue
		}
		renderCommand(&b, sub, []string{app.Name})
	}
	b.WriteString(shellIntegrationSection)

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}

func renderCommand(b *strings.Builder, c *cli.Command, parents []string) {
	full := make([]string, 0, len(parents)+1)
	full = append(full, parents...)
	full = append(full, c.Name)
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
			var labelSb67 strings.Builder
			for i, n := range names {
				if i > 0 {
					labelSb67.WriteString(", ")
				}
				if len(n) == 1 {
					labelSb67.WriteString("`-" + n + "`")
				} else {
					labelSb67.WriteString("`--" + n + "`")
				}
			}
			label += labelSb67.String()
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
