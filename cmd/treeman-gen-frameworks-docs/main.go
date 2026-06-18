// Command treeman-gen-frameworks-docs renders the built-in
// migration-framework preset table to the file named in os.Args[1],
// straight from framework.DefaultRegistry. Adding a preset updates the
// page with no hand-editing. Wired into `just sync-docs`.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/stubbedev/treeman/internal/migrations/framework"
)

func main() {
	reg := framework.DefaultRegistry()

	var b strings.Builder
	b.WriteString("# Framework presets\n\n")
	b.WriteString("[← back to docs](README.md)\n\n")
	b.WriteString("Auto-generated from `framework.DefaultRegistry` (the built-in\n")
	b.WriteString("migration-framework detectors). Run `just sync-docs` after adding a\n")
	b.WriteString("preset to refresh. `treeman fw detect` lists which of these match the\n")
	b.WriteString("current repo; declare your own under the `frameworks:` config block.\n\n")
	fmt.Fprintf(&b, "%d built-in detectors. Marker entries joined by `|` mean any-of.\n\n", len(reg.Specs))

	b.WriteString("| Framework | Marker(s) | Migration dir(s) | Engine hint |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, s := range reg.Specs {
		hint := s.EngineHint
		if hint == "" {
			hint = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			s.Name, codeJoin(s.Markers), codeJoin(s.MigrationDirs), hint)
	}
	b.WriteString("\nEvery matched migration file, lockfile, and dump is content-hashed\n")
	b.WriteString("(BLAKE3) into the snapshot fingerprint — there is no per-framework\n")
	b.WriteString("\"hash mode\"; any content/add/remove triggers a rebuild.\n")

	out := os.Args[1]
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}

// codeJoin renders a string slice as comma-separated `code` spans.
func codeJoin(xs []string) string {
	if len(xs) == 0 {
		return "—"
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = "`" + x + "`"
	}
	return strings.Join(out, ", ")
}
