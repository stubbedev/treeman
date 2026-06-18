package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
)

// TestRunErrorsWhenRunEmpty asserts that we fail loud instead of
// silently falling back to a hardcoded migrate command.
func TestRunErrorsWhenRunEmpty(t *testing.T) {
	_, err := Run(context.Background(), FromMigrate(config.Step{}), t.TempDir(), "anydb", template.Context{}, nil)
	if err == nil {
		t.Fatal("expected error for empty migrate.run, got nil")
	}
	if !strings.Contains(err.Error(), "migrate.run") {
		t.Errorf("error should mention migrate.run, got: %v", err)
	}
}

// TestRunErrorMentionsSeedLabelWhenSeed asserts that an empty seed
// spec produces a "seed.run is required" error rather than a
// "migrations.migrate.run" error — the label routes the diagnostic
// to whichever YAML block actually broke.
func TestRunErrorMentionsSeedLabelWhenSeed(t *testing.T) {
	_, err := Run(context.Background(), FromSeed(config.Step{}), t.TempDir(), "anydb", template.Context{}, nil)
	if err == nil {
		t.Fatal("expected error for empty seed.run, got nil")
	}
	if !strings.Contains(err.Error(), "seed.run") {
		t.Errorf("error should mention seed.run, got: %v", err)
	}
}

// TestRunSubstitutesTargetDB checks that {target_db} in env values is
// replaced with the resolved DB name before exec. Uses shell builtin
// `echo` so the test doesn't depend on PATH being populated.
func TestRunSubstitutesTargetDB(t *testing.T) {
	dir := t.TempDir()
	out, err := Run(context.Background(),
		FromMigrate(config.Step{
			Run: `echo "DB_DATABASE=$DB_DATABASE"; echo "DB_TEST_DATABASE=$DB_TEST_DATABASE"; echo "PLAIN=$PLAIN"; echo "TREEMAN_TARGET_DB=$TREEMAN_TARGET_DB"`,
			Env: map[string]string{
				"DB_DATABASE":      "{target_db}",
				"DB_TEST_DATABASE": "literal_{target_db}_suffix",
				"PLAIN":            "no-placeholder",
			},
		}),
		dir,
		"myapp_template_feature-x",
		template.Context{},
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit %d, stderr=%q", out.ExitCode, out.StderrTail)
	}
	if !strings.Contains(out.StdoutTail, "DB_DATABASE=myapp_template_feature-x") {
		t.Errorf("DB_DATABASE not substituted: %q", out.StdoutTail)
	}
	if !strings.Contains(out.StdoutTail, "DB_TEST_DATABASE=literal_myapp_template_feature-x_suffix") {
		t.Errorf("DB_TEST_DATABASE not substituted: %q", out.StdoutTail)
	}
	if !strings.Contains(out.StdoutTail, "PLAIN=no-placeholder") {
		t.Errorf("PLAIN literal lost: %q", out.StdoutTail)
	}
	if !strings.Contains(out.StdoutTail, "TREEMAN_TARGET_DB=myapp_template_feature-x") {
		t.Errorf("TREEMAN_TARGET_DB missing: %q", out.StdoutTail)
	}
}

// TestRunTeesStdoutAndStderrToLogPath asserts that when Spec.LogPath
// is set, the merged stdout+stderr stream lands on disk so a failure
// remains debuggable after the in-memory tail is gone. Regression
// guard for the prepare incident where Laravel migrate exit 1 wrote
// its diagnostic to stdout, daemon emitted only StderrTail (empty),
// and the failure reason was lost.
func TestRunTeesStdoutAndStderrToLogPath(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "deep", "nested", "migrate.log")
	spec := FromMigrate(config.Step{
		Run: `echo OUT-LINE; echo ERR-LINE 1>&2; exit 7`,
	}).WithLogPath(logPath)

	out, err := Run(context.Background(), spec, dir, "anydb", template.Context{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 7 {
		t.Fatalf("exit %d, want 7; stderr=%q stdout=%q", out.ExitCode, out.StderrTail, out.StdoutTail)
	}
	if out.LogPath != logPath {
		t.Errorf("Outcome.LogPath = %q, want %q", out.LogPath, logPath)
	}
	body, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if !strings.Contains(string(body), "OUT-LINE") {
		t.Errorf("log missing stdout content: %q", body)
	}
	if !strings.Contains(string(body), "ERR-LINE") {
		t.Errorf("log missing stderr content: %q", body)
	}
}

// TestFormatErrorIncludesStdoutTail guards the actual KON incident:
// `php artisan migrate` writes failure output to stdout via Symfony
// Console; the prepare error message used to interpolate only
// StderrTail and turned exit 1 into a literal empty diagnostic.
func TestFormatErrorIncludesStdoutTail(t *testing.T) {
	msg := FormatError("migrate source", "kontainer_testing_wt_x", Outcome{
		ExitCode:   1,
		StdoutTail: "SQLSTATE[42S02]: Base table or view not found",
		StderrTail: "",
		LogPath:    "/tmp/x.log",
	})
	if !strings.Contains(msg, "exit 1") {
		t.Errorf("message missing exit code: %q", msg)
	}
	if !strings.Contains(msg, "SQLSTATE[42S02]") {
		t.Errorf("stdout tail dropped (KON incident regression): %q", msg)
	}
	if !strings.Contains(msg, "/tmp/x.log") {
		t.Errorf("log path pointer missing: %q", msg)
	}
}

// TestFormatErrorBothStreams asserts both tails are surfaced when
// both are non-empty.
func TestFormatErrorBothStreams(t *testing.T) {
	msg := FormatError("seed source", "appdb", Outcome{
		ExitCode:   2,
		StdoutTail: "OUT",
		StderrTail: "ERR",
	})
	if !strings.Contains(msg, `stdout="OUT"`) || !strings.Contains(msg, `stderr="ERR"`) {
		t.Errorf("both streams should appear: %q", msg)
	}
}

// TestFormatErrorNoOutput keeps the diagnostic actionable even when
// nothing was captured (subprocess killed before printing, runaway
// fork, etc.) — without this branch the message would silently lose
// its trailing colon and look like a string-formatting bug.
func TestFormatErrorNoOutput(t *testing.T) {
	msg := FormatError("migrate source", "x", Outcome{ExitCode: 137})
	if !strings.Contains(msg, "<no output captured>") {
		t.Errorf("empty-output sentinel missing: %q", msg)
	}
}

// TestRunInheritsCwd asserts the subprocess runs with repoRoot as cwd.
func TestRunInheritsCwd(t *testing.T) {
	dir := t.TempDir()
	out, err := Run(context.Background(),
		FromMigrate(config.Step{Run: "pwd"}),
		dir,
		"anydb",
		template.Context{},
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit %d, stderr=%q", out.ExitCode, out.StderrTail)
	}
	got := strings.TrimSpace(out.StdoutTail)
	want, _ := filepath.EvalSymlinks(dir)
	gotAbs, _ := filepath.EvalSymlinks(got)
	if gotAbs != want {
		t.Errorf("cwd: got %q, want %q", got, want)
	}
}
