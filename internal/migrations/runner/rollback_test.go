package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/template"
)

// TestFromRollbackSetsStepsEnv asserts the step count is exposed to the
// command via TREEMAN_ROLLBACK_STEPS (Run is not template-rendered, so a
// brace placeholder couldn't carry it).
func TestFromRollbackSetsStepsEnv(t *testing.T) {
	spec := FromRollback(config.Step{Run: "true"}, 7)
	if spec.Label != "rollback" {
		t.Errorf("label = %q, want rollback", spec.Label)
	}
	if spec.ExtraEnv["TREEMAN_ROLLBACK_STEPS"] != "7" {
		t.Errorf("TREEMAN_ROLLBACK_STEPS = %q, want 7", spec.ExtraEnv["TREEMAN_ROLLBACK_STEPS"])
	}
}

// TestRollbackStepsVisibleToCommand runs the rollback command and
// confirms the env var reaches the subprocess.
func TestRollbackStepsVisibleToCommand(t *testing.T) {
	out, err := Run(context.Background(),
		FromRollback(config.Step{Run: `echo "STEPS=$TREEMAN_ROLLBACK_STEPS"`}, 3),
		t.TempDir(), "anydb", template.Context{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.ExitCode != 0 {
		t.Fatalf("exit = %d", out.ExitCode)
	}
	if !strings.Contains(out.StdoutTail, "STEPS=3") {
		t.Errorf("rollback command did not see step count; stdout=%q", out.StdoutTail)
	}
}

// TestRollbackSharesMigratePathInjection proves the rollback command
// runs with the exact same PATH as migrate — both flow through
// runner.Run → shellenv.BaseEnv, which merges the daemon's login-shell
// PATH. Equal, non-empty PATH for both specs guards against rollback
// ever bypassing that shared env injection.
func TestRollbackSharesMigratePathInjection(t *testing.T) {
	const echoPath = `printf 'PATH=%s\n' "$PATH"`
	mg, err := Run(context.Background(), FromMigrate(config.Step{Run: echoPath}),
		t.TempDir(), "anydb", template.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := Run(context.Background(), FromRollback(config.Step{Run: echoPath}, 1),
		t.TempDir(), "anydb", template.Context{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mg.StdoutTail == "" || !strings.HasPrefix(mg.StdoutTail, "PATH=") {
		t.Fatalf("migrate PATH not captured: %q", mg.StdoutTail)
	}
	if mg.StdoutTail != rb.StdoutTail {
		t.Errorf("rollback PATH differs from migrate:\n migrate=%q\n rollback=%q", mg.StdoutTail, rb.StdoutTail)
	}
}
