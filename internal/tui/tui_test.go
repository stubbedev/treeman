package tui

import (
	"errors"
	"fmt"
	"strings"
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
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "delete":
		return tea.KeyMsg{Type: tea.KeyDelete}
	case "home", "ctrl+a":
		return tea.KeyMsg{Type: tea.KeyHome}
	case "end", "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "ctrl+w":
		return tea.KeyMsg{Type: tea.KeyCtrlW}
	case "ctrl+u":
		return tea.KeyMsg{Type: tea.KeyCtrlU}
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

func TestMultiEnterNoMarksFallsBackToCursor(t *testing.T) {
	m := newModel([]string{"a", "b", "c"}, Options{Height: 10, Multi: true})
	feed(m, "down", "enter") // nothing marked; cursor on b
	r := m.result()
	if len(r.Indices) != 1 || r.Indices[0] != 1 {
		t.Errorf("indices = %v, want [1] (highlighted fallback)", r.Indices)
	}
}

func TestFooterSingleCancelHint(t *testing.T) {
	m := newModel([]string{"a"}, Options{Height: 10, Prompt: "switch/create", CancelHint: "wizard"})
	f := m.footer()
	if got := strings.Count(f, "ctrl+c"); got != 1 {
		t.Errorf("footer mentions ctrl+c %d times, want 1: %q", got, f)
	}
	if !strings.Contains(f, "ctrl+c wizard") {
		t.Errorf("footer missing cancel override: %q", f)
	}
}

func TestHighlightMatchSkipsStyledItems(t *testing.T) {
	styled := "\x1b[36m[worktree] foo\x1b[0m"
	m := newModel([]string{styled}, Options{Height: 10, Values: []string{"foo"}})
	m.input.SetValue("foo")
	m.refilter()
	if len(m.filtered) != 1 {
		t.Fatal("expected match via Values")
	}
	if got := m.highlightMatch(0); got != styled {
		t.Errorf("styled item mutated: %q", got)
	}
}

func TestPickerHeightEnv(t *testing.T) {
	t.Setenv("TREEMAN_PICKER_HEIGHT", "5")
	if h := pickerHeight(); h != 5 {
		t.Errorf("height = %d, want 5", h)
	}
	t.Setenv("TREEMAN_PICKER_HEIGHT", "nonsense")
	if h := pickerHeight(); h != defaultHeight {
		t.Errorf("height = %d, want default on junk", h)
	}
}

func TestCancel(t *testing.T) {
	m := newModel([]string{"a"}, Options{Height: 10})
	feed(m, "ctrl+c")
	if !m.canceled || m.aborted {
		t.Errorf("ctrl+c: canceled=%v aborted=%v, want canceled only", m.canceled, m.aborted)
	}
}

func TestEscAbortsWholeCommand(t *testing.T) {
	m := newModel([]string{"a"}, Options{Height: 10})
	feed(m, "esc")
	if !m.aborted || m.canceled {
		t.Errorf("esc: canceled=%v aborted=%v, want aborted only", m.canceled, m.aborted)
	}
	// ErrAborted must satisfy ErrCanceled checks so single-step
	// commands quit without every call site special-casing Esc.
	if !errors.Is(ErrAborted, ErrCanceled) {
		t.Error("ErrAborted should wrap ErrCanceled")
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

// The picker's query editing routes through bubbles/textinput, so it
// inherits caret movement and readline-style deletes. These tests pin
// the bindings callers now rely on (#37).
func TestCaretEditing(t *testing.T) {
	m := newModel([]string{"x"}, Options{Height: 10})
	feed(m, "a", "b", "c")
	feed(m, "left")
	feed(m, "X") // insert before the caret: abXc
	if got := m.input.Value(); got != "abXc" {
		t.Errorf("query = %q, want abXc", got)
	}
	feed(m, "home", "Y") // YabXc
	if got := m.input.Value(); got != "YabXc" {
		t.Errorf("query = %q, want YabXc", got)
	}
	feed(m, "end", "Z") // YabXcZ
	if got := m.input.Value(); got != "YabXcZ" {
		t.Errorf("query = %q, want YabXcZ", got)
	}
	// Forward delete at the caret removes the rune after it.
	feed(m, "home", "delete", "delete") // bXcZ
	if got := m.input.Value(); got != "bXcZ" {
		t.Errorf("query = %q, want bXcZ", got)
	}
}

func TestWordAndLineDeletes(t *testing.T) {
	m := newModel([]string{"x"}, Options{Height: 10})
	feed(m, "feature x")
	feed(m, "ctrl+w") // drop the last word, keep the space
	if got := m.input.Value(); got != "feature " {
		t.Errorf("after ctrl+w query = %q, want %q", got, "feature ")
	}
	feed(m, "ctrl+u") // clear to start
	if got := m.input.Value(); got != "" {
		t.Errorf("after ctrl+u query = %q, want empty", got)
	}
}

func TestEditingRefiltersLive(t *testing.T) {
	m := newModel([]string{"feature/a", "bugfix/b"}, Options{Height: 10})
	feed(m, "f", "e", "a") // feature/a
	if len(m.filtered) != 1 || m.filtered[0] != 0 {
		t.Fatalf("filtered = %v, want [0]", m.filtered)
	}
	// Backspacing down to "fe" still leaves feature/a; to "f" both.
	feed(m, "backspace")
	if len(m.filtered) != 1 {
		t.Fatalf("after one backspace filtered = %v", m.filtered)
	}
	feed(m, "backspace")
	if len(m.filtered) != 2 {
		t.Errorf("after two backspaces filtered = %v, want both", m.filtered)
	}
}

func TestListPaging(t *testing.T) {
	items := make([]string, 25)
	for i := range items {
		items[i] = fmt.Sprintf("item%02d", i)
	}
	m := newModel(items, Options{Height: 10})
	feed(m, "pgdown")
	if m.cursor != 10 {
		t.Errorf("after pgdown cursor = %d, want 10", m.cursor)
	}
	feed(m, "pgup")
	if m.cursor != 0 {
		t.Errorf("after pgup cursor = %d, want 0", m.cursor)
	}
	feed(m, "pgup") // clamps at 0
	if m.cursor != 0 {
		t.Errorf("pgup at top: cursor = %d, want 0", m.cursor)
	}
	for range 5 {
		feed(m, "pgdown")
	}
	if m.cursor != len(items)-1 {
		t.Errorf("pgdn overflow: cursor = %d, want %d", m.cursor, len(items)-1)
	}
}

func TestViewRendersCaretAtPosition(t *testing.T) {
	m := newModel([]string{"x"}, Options{Height: 10})
	feed(m, "a", "b", "c", "left")
	// Color is off in tests, so ui.Dim is identity: the prompt line
	// literally contains the caret before the final rune.
	if !strings.Contains(m.View(), "ab▌c") {
		t.Errorf("view missing caret at position: %q", m.View())
	}
}
