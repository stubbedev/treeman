// Command treeman is the CLI half of the treeman per-worktree DB
// orchestrator. The full subcommand tree is being ported from the
// Rust workspace at `crates/treeman-cli/src/main.rs` and is currently
// stubbed; only `--version` is wired.
//
// The Rust workspace stays in-tree as a read-only spec until the Go
// port reaches feature parity (see plan in
// ~/.claude/plans/for-both-setup-and-resilient-meerkat.md).
package main

import (
	"fmt"
	"os"

	"github.com/stubbedev/treeman/internal/version"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Printf("treeman %s\n", version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "treeman: Go rewrite in progress — CLI surface not wired yet")
	fmt.Fprintln(os.Stderr, "         Run `treeman --version` to see the build.")
	os.Exit(64) // EX_USAGE
}
