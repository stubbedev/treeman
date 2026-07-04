package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// key builds a KeyMsg the model's Update understands. Multi-char
// strings that aren't a known special key are treated as typed runes.
func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// feed replays a sequence of keys through the model.
func feed(m *model, seq ...string) {
	for _, s := range seq {
		m.Update(key(s))
	}
}

func TestSelectSingle(t *testing.T) {
	m := newModel([]string{"alpha", "beta", "gamma"}, Options{Height: 10})
	feed(m, "down", "enter")
	if m.canceled {
		t.Fatal("unexpected cancel")
	}
	r := m.result()
	if r.Index != 1 || len(r.Indices) != 1 || r.Indices[0] != 1 {
		t.Errorf("result = %+v, want index 1", r)
	}
}

func TestFilterNarrows(t *testing.T) {
	m := newModel([]string{"feature/a", "bugfix/b", "feature/c"}, Options{Height: 10})
	feed(m, "b", "u", "g") // "bug" → only bugfix/b
	if len(m.filtered) != 1 || m.filtered[0] != 1 {
		t.Fatalf("filtered = %v, want [1]", m.filtered)
	}
	feed(m, "enter")
	if m.result().Index != 1 {
		t.Errorf("index = %d, want 1", m.result().Index)
	}
}

func TestTypedNewNoMatch(t *testing.T) {
	m := newModel([]string{"alpha", "beta"}, Options{Height: 10})
	feed(m, "z", "z", "z") // matches nothing
	feed(m, "enter")
	r := m.result()
	if r.Index != -1 {
		t.Errorf("index = %d, want -1 (no match)", r.Index)
	}
	if r.Query != "zzz" {
		t.Errorf("query = %q, want zzz", r.Query)
	}
}

func TestMultiSelect(t *testing.T) {
	m := newModel([]string{"a", "b", "c", "d"}, Options{Height: 10, Multi: true})
	// mark a (tab advances to b), skip to c, mark c.
	feed(m, "tab", "down", "tab", "enter")
	r := m.result()
	if len(r.Indices) != 2 || r.Indices[0] != 0 || r.Indices[1] != 2 {
		t.Errorf("indices = %v, want [0 2]", r.Indices)
	}
}

func TestToggleAll(t *testing.T) {
	m := newModel([]string{"a", "b", "c"}, Options{Height: 10, Multi: true})
	feed(m, "shift+tab", "enter")
	if len(m.result().Indices) != 3 {
		t.Errorf("toggle-all should mark all 3, got %v", m.result().Indices)
	}
}

func TestAction(t *testing.T) {
	opts := Options{Height: 10, Actions: []Action{{Key: "ctrl+x", Name: "cherry-pick", NeedsSelection: true}}}
	m := newModel([]string{"c1", "c2"}, opts)
	feed(m, "down", "ctrl+x")
	r := m.result()
	if r.Action != "cherry-pick" {
		t.Errorf("action = %q, want cherry-pick", r.Action)
	}
	if r.Index != 1 {
		t.Errorf("index = %d, want 1", r.Index)
	}
}

func TestCancel(t *testing.T) {
	m := newModel([]string{"a"}, Options{Height: 10})
	feed(m, "ctrl+c")
	if !m.canceled {
		t.Error("expected canceled")
	}
}

func TestBackspace(t *testing.T) {
	m := newModel([]string{"apple", "apricot", "cherry"}, Options{Height: 10})
	feed(m, "a", "p", "p") // "app" → apple only
	if len(m.filtered) != 1 {
		t.Fatalf("filtered = %v", m.filtered)
	}
	feed(m, "backspace", "backspace") // "a" → apple, apricot (cherry has no 'a')
	if len(m.filtered) != 2 {
		t.Errorf("after backspace filtered = %v, want 2", m.filtered)
	}
}
