// Colored help output for the urfave/cli v3 surface. The default
// templates print monochrome; this file overrides them so headings,
// command names, and flag specs match the look used by the sibling
// `srv` CLI.
//
// Strategy:
//   - Section headings + command names are colored via template
//     funcs. Their column alignment is computed by `offset`/`indent`
//     on the uncolored string, so the inserted ANSI bytes don't
//     shift visible positions.
//   - Flag specs are colored *after* tabwriter has aligned them, by
//     buffering the rendered help and post-processing flag lines.
//     This avoids tabwriter miscounting embedded ANSI bytes.
//
// All coloring goes through internal/ui, which already honors
// NO_COLOR, FORCE_COLOR, TERM=dumb, and TTY detection.
package cmd

import (
	"bytes"
	"io"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/stubbedev/treeman/internal/ui"
)

func init() {
	setupColoredHelp()
}

func setupColoredHelp() {
	heading := func(s string) string { return ui.Bold(ui.Yellow(s)) }
	command := func(s string) string { return ui.Bold(ui.Green(s)) }

	funcs := map[string]any{
		"styleHeading": heading,
		"styleCommand": command,
		"styleCyan":    ui.Cyan,
	}

	cli.HelpPrinter = func(out io.Writer, templ string, data any) {
		var buf bytes.Buffer
		cli.HelpPrinterCustom(&buf, templ, data, funcs)
		_, _ = io.WriteString(out, colorizeFlagLines(buf.String()))
	}

	cli.RootCommandHelpTemplate = coloredRootHelpTemplate
	cli.CommandHelpTemplate = coloredCommandHelpTemplate
	cli.SubcommandHelpTemplate = coloredSubcommandHelpTemplate
}

// colorizeFlagLines walks the rendered help line by line and colors
// the flag-spec portion of any line that looks like a flag entry
// (indented and starting with `-`). Runs after tabwriter has aligned
// the columns so the inserted ANSI bytes don't shift descriptions.
func colorizeFlagLines(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 64)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if isFlagLine(line) {
			b.WriteString(colorFlagSpec(line))
		} else {
			b.WriteString(line)
		}
		if i != len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func isFlagLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(trimmed, "-")
}

// colorFlagSpec wraps everything up to the description column in
// the flag style. The description column boundary is detected by
// the first run of two or more consecutive spaces (tabwriter's
// post-alignment marker).
func colorFlagSpec(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	leading := line[:i]
	rest := line[i:]

	descStart := -1
	j := 0
	for j < len(rest) {
		if rest[j] == ' ' {
			k := j
			for k < len(rest) && rest[k] == ' ' {
				k++
			}
			if k-j >= 2 {
				descStart = j
				break
			}
			j = k
			continue
		}
		j++
	}

	style := func(s string) string { return ui.Bold(ui.Red(s)) }
	if descStart < 0 {
		return leading + style(rest)
	}
	return leading + style(rest[:descStart]) + rest[descStart:]
}

// Templates below are forked from urfave/cli v3's defaults. Headings
// gain styleHeading, command names gain styleCommand. The
// visibleCommandTemplate body is inlined (rather than referenced via
// {{template ...}}) because DefaultPrintHelpCustom re-parses the
// package-level subtemplates after our root template, which would
// overwrite any redefines.

const coloredRootHelpTemplate = `{{styleHeading "NAME:"}}

   {{template "helpNameTemplate" .}}

{{styleHeading "USAGE:"}}

   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}} {{if .VisibleFlags}}[global options]{{end}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Version}}{{if not .HideVersion}}

{{styleHeading "VERSION:"}}

   {{.Version}}{{end}}{{end}}{{if .Description}}

{{styleHeading "DESCRIPTION:"}}

   {{template "descriptionTemplate" .}}{{end}}
{{- if len .Authors}}

{{styleHeading "AUTHOR"}}{{template "authorsTemplate" .}}{{end}}{{if .VisibleCommands}}

{{styleHeading "COMMANDS:"}}
{{range .VisibleCategories}}{{if .Name}}
   {{styleHeading .Name}}:{{range .VisibleCommands}}
     {{styleCommand (join .Names ", ")}}{{"\t"}}{{.Usage}}{{end}}{{else}}{{ $cv := offsetCommands .VisibleCommands 5}}{{range .VisibleCommands}}
   {{$s := join .Names ", "}}{{styleCommand $s}}{{ $sp := subtract $cv (offset $s 3) }}{{ indent $sp ""}}{{wrap .Usage $cv}}{{end}}{{end}}{{end}}{{end}}{{if .VisibleFlagCategories}}

{{styleHeading "GLOBAL OPTIONS:"}}
{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{styleHeading "GLOBAL OPTIONS:"}}
{{template "visibleFlagTemplate" .}}{{end}}{{if .Copyright}}

{{styleHeading "COPYRIGHT:"}}

   {{template "copyrightTemplate" .}}{{end}}{{if .VisibleCommands}}

Use {{styleCyan (printf "%q" (print .FullName " [command] --help"))}} for more information about a command.{{end}}
`

const coloredCommandHelpTemplate = `{{styleHeading "NAME:"}}

   {{template "helpNameTemplate" .}}

{{styleHeading "USAGE:"}}

   {{template "usageTemplate" .}}{{if .Category}}

{{styleHeading "CATEGORY:"}}

   {{.Category}}{{end}}{{if .Description}}

{{styleHeading "DESCRIPTION:"}}

   {{template "descriptionTemplate" .}}{{end}}{{if .VisibleFlagCategories}}

{{styleHeading "OPTIONS:"}}
{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{styleHeading "OPTIONS:"}}
{{template "visibleFlagTemplate" .}}{{end}}{{if .VisiblePersistentFlags}}

{{styleHeading "GLOBAL OPTIONS:"}}
{{template "visiblePersistentFlagTemplate" .}}{{end}}
`

const coloredSubcommandHelpTemplate = `{{styleHeading "NAME:"}}

   {{template "helpNameTemplate" .}}

{{styleHeading "USAGE:"}}

   {{if .UsageText}}{{wrap .UsageText 3}}{{else}}{{.FullName}}{{if .VisibleCommands}} [command [command options]]{{end}}{{if .ArgsUsage}} {{.ArgsUsage}}{{else}}{{if .Arguments}} [arguments...]{{end}}{{end}}{{end}}{{if .Category}}

{{styleHeading "CATEGORY:"}}

   {{.Category}}{{end}}{{if .Description}}

{{styleHeading "DESCRIPTION:"}}

   {{template "descriptionTemplate" .}}{{end}}{{if .VisibleCommands}}

{{styleHeading "COMMANDS:"}}
{{ $cv := offsetCommands .VisibleCommands 5}}{{range .VisibleCommands}}
   {{$s := join .Names ", "}}{{styleCommand $s}}{{ $sp := subtract $cv (offset $s 3) }}{{ indent $sp ""}}{{wrap .Usage $cv}}{{end}}{{end}}{{if .VisibleFlagCategories}}

{{styleHeading "OPTIONS:"}}
{{template "visibleFlagCategoryTemplate" .}}{{else if .VisibleFlags}}

{{styleHeading "OPTIONS:"}}
{{template "visibleFlagTemplate" .}}{{end}}{{if .VisibleCommands}}

Use {{styleCyan (printf "%q" (print .FullName " [command] --help"))}} for more information about a command.{{end}}
`
