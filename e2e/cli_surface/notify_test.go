//go:build e2e

package cli_surface_e2e

import (
	"strings"
	"testing"
)

// TestNotifyTestBackendNoneErrors asserts `treeman notify test
// --backend none` refuses with a clear message rather than pretending to
// send — deterministic regardless of whether notify-send / osascript is
// installed on the host.
func TestNotifyTestBackendNoneErrors(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, t.TempDir(), "notify", "test", "--backend", "none")
	if res.err == nil {
		t.Fatalf("expected non-zero exit for backend=none; stdout=%q", res.stdout)
	}
	if !strings.Contains(res.stderr, "none") {
		t.Errorf("stderr should explain the none backend, got: %q", res.stderr)
	}
}

// TestNotifyHelpListsTest asserts the `test` subcommand is wired into the
// `notify` group so it shows up in help (and gen-docs).
func TestNotifyHelpListsTest(t *testing.T) {
	e := newEnv(t)
	res := e.run(t, t.TempDir(), "notify", "--help")
	out := res.stdout + res.stderr
	if !strings.Contains(out, "test") {
		t.Errorf("`notify --help` should list the `test` subcommand, got: %q", out)
	}
}

// TestNotifyTestUnavailableBackendErrors asserts that forcing a backend
// whose binary is absent fails with an actionable message. Uses
// osascript on non-macOS hosts (and notify-send where osascript is the
// native one) — whichever is NOT present. Skips if both happen to exist.
func TestNotifyTestUnavailableBackendErrors(t *testing.T) {
	e := newEnv(t)
	// Probe both; pick one that's unavailable so the assertion is real.
	for _, backend := range []string{"osascript", "notify-send"} {
		res := e.run(t, t.TempDir(), "notify", "test", "--backend", backend)
		if res.err != nil && strings.Contains(res.stderr, "not available") {
			return // got the expected unavailable-backend error
		}
	}
	t.Skip("both notify-send and osascript appear available on this host")
}
