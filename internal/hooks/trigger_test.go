package hooks

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stubbedev/treeman/internal/config"
)

func TestTriggersAreTheHooksConfigSections(t *testing.T) {
	typ := reflect.TypeFor[config.HooksConfig]()
	keys := make([]string, typ.NumField())
	for i := range typ.NumField() {
		keys[i], _, _ = strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
	}
	names := make([]string, 0, len(Triggers()))
	for _, tr := range Triggers() {
		names = append(names, tr.String())
	}
	if !slices.Equal(keys, names) {
		t.Fatalf("hooks: YAML keys %v, triggers %v", keys, names)
	}
}

func TestActionsSelectsTheTriggersSection(t *testing.T) {
	h := config.HooksConfig{
		OnCreateBeforeEngines: []config.Action{{Run: []string{"create-before-engines"}}},
		OnCreateAfterEngines:  []config.Action{{Run: []string{"create-after-engines"}}},
		OnDeleteBeforeEngines: []config.Action{{Run: []string{"delete-before-engines"}}},
		OnDeleteAfterEngines:  []config.Action{{Run: []string{"delete-after-engines"}}},
		OnCheckout:            []config.Action{{Run: []string{"checkout"}}},
		OnFileChange: []config.FilteredAction{
			{Match: []string{"migrations"}, Run: []string{"file-change"}},
		},
	}
	for _, tr := range Triggers() {
		got := tr.Actions(&h)
		if len(got) != 1 || len(got[0].Run) != 1 || got[0].Run[0] != tr.String() {
			t.Errorf("%s.Actions = %+v", tr, got)
		}
	}
}

func TestParseTriggerRoundTripsEveryName(t *testing.T) {
	for _, tr := range Triggers() {
		got, err := ParseTrigger(tr.String())
		if err != nil || got != tr {
			t.Errorf("ParseTrigger(%q) = %v, %v", tr, got, err)
		}
	}
}

func TestParseTriggerRejectsUnknownNameListingValidOnes(t *testing.T) {
	_, err := ParseTrigger("create-befor-engines")
	if err == nil {
		t.Fatal("unknown trigger accepted")
	}
	for _, tr := range Triggers() {
		if !strings.Contains(err.Error(), tr.String()) {
			t.Errorf("error %q does not list %s", err, tr)
		}
	}
}

func TestParseManualTriggerRejectsFileChange(t *testing.T) {
	_, err := ParseManualTrigger(FileChange.String())
	if err == nil {
		t.Fatal("file-change accepted as an on-demand trigger")
	}
	if !strings.Contains(err.Error(), JoinTriggers(ManualTriggers())) {
		t.Errorf("error %q does not list the on-demand triggers", err)
	}
	if got, err := ParseManualTrigger(Checkout.String()); err != nil || got != Checkout {
		t.Errorf("ParseManualTrigger(checkout) = %v, %v", got, err)
	}
}

func TestTriggerRoundTripsThroughJSONAsItsName(t *testing.T) {
	b, err := json.Marshal(map[string]Trigger{"phase": DeleteAfterEngines})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"phase":"delete-after-engines"}` {
		t.Errorf("got %s", b)
	}
	var back map[string]Trigger
	if err := json.Unmarshal(b, &back); err != nil || back["phase"] != DeleteAfterEngines {
		t.Errorf("decoded %v, %v", back, err)
	}
}

func TestTriggerJSONRejectsUnknownName(t *testing.T) {
	var tr Trigger
	if err := json.Unmarshal([]byte(`"teardown"`), &tr); err == nil {
		t.Fatalf("decoded unknown trigger as %s", tr)
	}
}
