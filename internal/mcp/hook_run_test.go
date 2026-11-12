package mcp

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/hooks"
)

func TestHookToolRejectsTriggersThatCannotRunOnDemand(t *testing.T) {
	for _, phase := range []string{"create-befor-engines", "setup", hooks.FileChange.String(), ""} {
		_, _, err := hookTool(context.Background(), nil, hookIn{Phase: phase, Worktree: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), hooks.JoinTriggers(hooks.ManualTriggers())) {
			t.Errorf("phase %q: got %v, want an error listing the on-demand triggers", phase, err)
		}
	}
}

func TestHookToolSchemaListsTheOnDemandTriggers(t *testing.T) {
	field, ok := reflect.TypeFor[hookIn]().FieldByName("Phase")
	if !ok {
		t.Fatal("hookIn has no Phase field")
	}
	if got, want := field.Tag.Get("jsonschema"), hooks.JoinTriggers(hooks.ManualTriggers()); got != want {
		t.Errorf("hook_run phase schema %q, want %q", got, want)
	}
}
