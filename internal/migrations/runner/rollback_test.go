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
