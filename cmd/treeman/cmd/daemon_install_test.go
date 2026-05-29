// Cross-platform coverage for `treeman daemon install` / `uninstall`.
//
// These paths write a systemd-user unit (Linux) or a LaunchAgent plist
// (macOS) and then run `systemctl` / `launchctl` against the real
// per-user service manager — which a test must never mutate. To get the
// real install/uninstall code under test on both OSes without that side
// effect, we:
//
//   - point HOME at a temp dir so the unit/plist lands in a sandbox, and
//   - put recording fakes for systemctl / launchctl / treemand on PATH
//     so the real argv is constructed and captured, not executed for real.
//
// daemonInstallSystemd/Launchd are called directly (rather than through
// the runtime.GOOS dispatcher) so BOTH branches run regardless of host —
// a Linux CI runner verifies the launchd plist, and a macOS runner
// verifies the systemd unit. The tests are un-tagged so the CI matrix
// (ubuntu-latest + macos-latest) runs them under plain `go test ./...`.
package cmd

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/daemonctl"
)

// sandbox redirects HOME to a temp dir and puts recording fakes for the
// named service-manager commands (plus a fake `treemand`, so ExecStart
// resolves deterministically) on PATH. Returns the home dir and a
// reader over the recorded "<name> <argv>" lines.
func sandbox(t *testing.T, names ...string) (home string, calls func() string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "calls.log")
	for _, name := range append([]string{"treemand"}, names...) {
		script := "#!/bin/sh\n" +
			"printf '%s %s\\n' \"$(basename \"$0\")\" \"$*\" >> \"" + logPath + "\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return home, func() string {
		b, _ := os.ReadFile(logPath)
		return string(b)
	}
}

func TestDaemonInstallSystemdWritesUnitAndEnables(t *testing.T) {
	home, calls := sandbox(t, "systemctl")

	if err := daemonInstallSystemd(context.Background()); err != nil {
		t.Fatalf("daemonInstallSystemd: %v", err)
	}

	unitPath := filepath.Join(home, ".config", "systemd", "user", "treemand.service")
	b, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	unit := string(b)
	for _, want := range []string{
		"[Install]", "WantedBy=default.target",
		"[Service]", "ExecStart=", "Restart=on-failure", "Type=simple",
		"[Unit]", "Description=Treeman per-worktree DB orchestrator daemon",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}
	// ExecStart must point at the resolved treemand binary, not a bare name.
	if !strings.Contains(unit, "ExecStart=/") {
		t.Errorf("ExecStart should be an absolute path:\n%s", unit)
	}

	got := calls()
	if !strings.Contains(got, "systemctl --user daemon-reload") {
		t.Errorf("expected daemon-reload, got:\n%s", got)
	}
	if !strings.Contains(got, "systemctl --user enable --now treemand") {
		t.Errorf("expected enable --now, got:\n%s", got)
	}
}

func TestDaemonUninstallSystemdDisablesAndRemoves(t *testing.T) {
	home, calls := sandbox(t, "systemctl")

	unitPath := filepath.Join(home, ".config", "systemd", "user", "treemand.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := daemonUninstallSystemd(context.Background()); err != nil {
		t.Fatalf("daemonUninstallSystemd: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file should have been removed, stat err = %v", err)
	}
	got := calls()
	if !strings.Contains(got, "systemctl --user disable --now treemand") {
		t.Errorf("expected disable --now, got:\n%s", got)
	}
	if !strings.Contains(got, "systemctl --user daemon-reload") {
		t.Errorf("expected daemon-reload, got:\n%s", got)
	}
}

func TestDaemonInstallLaunchdWritesPlistAndBootstraps(t *testing.T) {
	home, calls := sandbox(t, "launchctl")

	if err := daemonInstallLaunchd(context.Background()); err != nil {
		t.Fatalf("daemonInstallLaunchd: %v", err)
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", daemonctl.LaunchdLabel+".plist")
	b, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	plist := string(b)

	// Must be well-formed XML.
	if err := xml.Unmarshal(b, new(struct {
		XMLName xml.Name `xml:"plist"`
	})); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, plist)
	}
	for _, want := range []string{
		"<key>Label</key>", daemonctl.LaunchdLabel,
		"<key>ProgramArguments</key>", "<key>RunAtLoad</key>",
		"<key>KeepAlive</key>", "<key>SuccessfulExit</key>",
		"<key>StandardOutPath</key>", "treemand.log",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	got := calls()
	uid := os.Getuid()
	domain := "gui/" + itoa(uid)
	if !strings.Contains(got, "launchctl bootstrap "+domain) {
		t.Errorf("expected launchctl bootstrap %s, got:\n%s", domain, got)
	}
	if !strings.Contains(got, "launchctl enable "+domain+"/"+daemonctl.LaunchdLabel) {
		t.Errorf("expected launchctl enable, got:\n%s", got)
	}
}

func TestDaemonUninstallLaunchdBootsOutAndRemoves(t *testing.T) {
	home, calls := sandbox(t, "launchctl")

	plistPath := filepath.Join(home, "Library", "LaunchAgents", daemonctl.LaunchdLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := daemonUninstallLaunchd(context.Background()); err != nil {
		t.Fatalf("daemonUninstallLaunchd: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist should have been removed, stat err = %v", err)
	}
	if got := calls(); !strings.Contains(got, "launchctl bootout gui/") {
		t.Errorf("expected launchctl bootout, got:\n%s", got)
	}
}

// itoa avoids pulling in strconv for a single positive-int format.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
