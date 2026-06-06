// Package safego runs goroutines with panic recovery so a runtime
// error in one async task can't take down a long-lived process (the
// daemon, in particular). Every `go` in treeman that runs unattended
// work routes through here instead of a bare `go func()`.
package safego

import (
	"fmt"
	"log/slog"
)

// Go runs fn in a new goroutine, recovering any panic and logging it
// with label + detail. label is a stable goroutine-role identifier in
// the project's colon-hierarchy convention (e.g. "watcher:fs",
// "plan:run"); detail is an optional disambiguator such as the
// worktree/repo path the goroutine acts on ("" when process-wide).
func Go(label, detail string, fn func()) {
	go func() {
		defer Recover(label, detail)
		fn()
	}()
}

// Recover is the deferred panic handler Go installs. Exported so a
// goroutine that must own its own `defer wg.Done()` ordering (e.g. a
// WaitGroup lane) can install recovery inline:
//
//	go func() { defer wg.Done(); defer safego.Recover(label, detail); ... }()
func Recover(label, detail string) {
	if r := recover(); r != nil {
		slog.Error("goroutine panic", "label", label, "detail", detail, "panic", fmt.Sprint(r))
	}
}
