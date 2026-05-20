// Command treemand is the daemon half of the treeman per-worktree DB
// orchestrator. It owns the per-repo file watchers, the SQLite event
// log, and the JSON-line RPC socket the CLI talks to.
//
// Currently stubbed during the Go rewrite. The Rust implementation
// at `crates/treeman-daemon/src/main.rs` is the spec.
package main

import (
	"fmt"
	"os"

	"github.com/stubbedev/treeman/internal/version"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Printf("treemand %s\n", version.Version)
		return
	}
	fmt.Fprintln(os.Stderr, "treemand: Go rewrite in progress — daemon not wired yet")
	os.Exit(64)
}
