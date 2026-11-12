package hooks

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stubbedev/treeman/internal/config"
)

// Trigger is one `hooks:` section of .treeman.yaml: the event that fires
// its actions and the phase its runs, events and logs are recorded under.
type Trigger uint8

// The triggers, in the order their sections appear in config.HooksConfig.
const (
	CreateBeforeEngines Trigger = iota
	CreateAfterEngines
	DeleteBeforeEngines
	DeleteAfterEngines
	Checkout
	FileChange
)

// Triggers returns every trigger in declaration order.
func Triggers() []Trigger {
	return []Trigger{CreateBeforeEngines, CreateAfterEngines, DeleteBeforeEngines, DeleteAfterEngines, Checkout, FileChange}
}

// ManualTriggers returns the triggers an operator can run on demand.
// FileChange is not one of them: its actions run per watched-path event.
func ManualTriggers() []Trigger {
	return []Trigger{CreateBeforeEngines, CreateAfterEngines, DeleteBeforeEngines, DeleteAfterEngines, Checkout}
}

// String is the trigger's YAML key under `hooks:` and its recorded phase.
func (t Trigger) String() string {
	switch t {
	case CreateBeforeEngines:
		return "create-before-engines"
	case CreateAfterEngines:
		return "create-after-engines"
	case DeleteBeforeEngines:
		return "delete-before-engines"
	case DeleteAfterEngines:
		return "delete-after-engines"
	case Checkout:
		return "checkout"
	case FileChange:
		return "file-change"
	default:
		return fmt.Sprintf("Trigger(%d)", uint8(t))
	}
}

// MarshalText encodes the trigger as its name.
func (t Trigger) MarshalText() ([]byte, error) {
	return []byte(t.String()), nil
}

// UnmarshalText decodes a trigger name, rejecting anything that is not one.
func (t *Trigger) UnmarshalText(text []byte) error {
	parsed, err := ParseTrigger(string(text))
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// Actions returns the actions configured for the trigger. For FileChange
// that is every file-change action regardless of its `match:` labels.
func (t Trigger) Actions(h *config.HooksConfig) []config.Action {
	switch t {
	case CreateBeforeEngines:
		return h.OnCreateBeforeEngines
	case CreateAfterEngines:
		return h.OnCreateAfterEngines
	case DeleteBeforeEngines:
		return h.OnDeleteBeforeEngines
	case DeleteAfterEngines:
		return h.OnDeleteAfterEngines
	case Checkout:
		return h.OnCheckout
	case FileChange:
		out := make([]config.Action, len(h.OnFileChange))
		for i, fa := range h.OnFileChange {
			out[i] = fa.Action
		}
		return out
	default:
		panic(fmt.Sprintf("hooks: actions of unknown %s", t))
	}
}

// LogPath is where group i of a run of t logs inside logDir.
func (t Trigger) LogPath(logDir string, i int) string {
	return filepath.Join(logDir, fmt.Sprintf("%s-%d.log", t, i))
}

// ParseTrigger resolves a trigger name, rejecting anything that is not one.
func ParseTrigger(name string) (Trigger, error) {
	return parseAmong(name, Triggers())
}

// ParseManualTrigger resolves the name of a trigger that can run on
// demand, rejecting any other name.
func ParseManualTrigger(name string) (Trigger, error) {
	return parseAmong(name, ManualTriggers())
}

// JoinTriggers renders ts as `a|b|c`.
func JoinTriggers(ts []Trigger) string {
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.String()
	}
	return strings.Join(names, "|")
}

func parseAmong(name string, valid []Trigger) (Trigger, error) {
	for _, t := range valid {
		if t.String() == name {
			return t, nil
		}
	}
	return 0, fmt.Errorf("unknown hook trigger %q (want %s)", name, JoinTriggers(valid))
}
