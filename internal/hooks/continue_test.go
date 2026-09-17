package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestContinueOnErrorWaitsAndPreservesFailure(t *testing.T) {
	root := t.TempDir()
	out, err := RunHooks(context.Background(), "create-before-engines", []config.Action{
		{},
		{Run: []string{"sleep 0.05; echo lockfile-invalid >&2; false", "touch must-not-run"}, ContinueOnError: true},
		{Run: []string{"sleep 0.1; touch backend-ready"}},
	}, root, root, "slug", false, emptyEnv(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Groups) != 2 {
		t.Fatalf("groups = %d", len(out.Groups))
	}
	failed := out.Groups[0]
	if failed.ExitCode != 1 || !failed.ContinueOnError || !strings.Contains(failed.StderrTail, "lockfile-invalid") ||
		!strings.Contains(string(failed.LogBody), "lockfile-invalid") {
		t.Fatalf("failure lost: %+v", failed)
	}
	if out.Groups[1].ContinueOnError {
		t.Fatal("policy leaked to sibling action")
	}
	if err := FirstFailureError("create-before-engines", out, nil); err != nil {
		t.Fatalf("optional exit was fatal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "backend-ready")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "must-not-run")); !os.IsNotExist(err) {
		t.Fatalf("remaining action steps ran: %v", err)
	}
}

func TestContinueOnErrorDoesNotHideRequiredFailure(t *testing.T) {
	out := RunOutcome{Groups: []GroupOutcome{
		{Command: "npm ci", ExitCode: 1, ContinueOnError: true},
		{Command: "composer install", ExitCode: 7, StderrTail: "required failed"},
	}}
	err := FirstFailureError("create-before-engines", out, []int64{10, 20})
	if err == nil {
		t.Fatal("required failure ignored")
	}
	for _, want := range []string{"composer install", "exited 7", "required failed", "--show 20"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
}

func TestContinueOnErrorDoesNotHideLaunchFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".treeman-hooks", "create-before-engines-0.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := RunHooks(context.Background(), "create-before-engines", []config.Action{
		{Run: []string{"true"}, ContinueOnError: true},
	}, root, root, "slug", false, emptyEnv(), true)
	if err == nil {
		t.Fatal("launch error ignored")
	}
}

func TestContinueOnErrorDoesNotHideRenderFailure(t *testing.T) {
	root := t.TempDir()
	_, err := RunHooks(context.Background(), "create-before-engines", []config.Action{
		{Run: []string{"true"}, ContinueOnError: true, ComposeService: "frontend", Engine: filepath.Join(root, "missing-engine")},
	}, root, root, "slug", false, emptyEnv(), true)
	if err == nil || !strings.Contains(err.Error(), "container resolve") {
		t.Fatalf("render error ignored: %v", err)
	}
}
