package runner

import (
	"context"
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
