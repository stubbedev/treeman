// Package shellenvtest provides test doubles for code that runs user
// commands through shellenv.CommandEnv.
package shellenvtest

import (
	"os"
	"path/filepath"
	"testing"
)

// FakeDirenv writes a `direnv` executable running script under /bin/sh
// into a fresh directory and returns that directory, to be put on the
// PATH handed to shellenv.CommandEnv.
func FakeDirenv(t *testing.T, script string) string {
	t.Helper()
	return FakeTool(t, "direnv", script)
}

// FakeTool writes an executable named name running script under
// /bin/sh into a fresh directory and returns that directory.
func FakeTool(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return dir
}

// FlakeDirenv returns the PATH entry of a fake direnv that, like
// `use flake`, prepends toolDir to PATH for whatever directory it is
// run in.
func FlakeDirenv(t *testing.T, toolDir string) string {
	t.Helper()
	return FakeDirenv(t, `[ "$1 $2" = "export json" ] || exit 2
printf '{"PATH":"`+toolDir+`:%s"}' "$PATH"
`)
}
