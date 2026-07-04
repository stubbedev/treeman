package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/stubbedev/treeman/internal/ui"
)

// InputOptions configure a single-line text prompt.
type InputOptions struct {
	Prompt      string // hint text, e.g. "commit message"
	Initial     string // pre-filled value
	Placeholder string
}

// Input runs a single-line text prompt and returns the entered string.
// Enter accepts (empty is allowed — the caller decides what "" means);
// Ctrl+C / Esc returns ErrCanceled. UI renders to stderr, keys from
// stdin, matching the pickers.
func Input(opts InputOptions) (string, error) {
	if !interactive() {
		return "", ErrNotTTY
	}
	ti := textinput.New()
	ti.Prompt = ui.Cyan("(" + hintFor(opts.Prompt) + ")> ")
	ti.Placeholder = opts.Placeholder
	ti.SetValue(opts.Initial)
	ti.CursorEnd()
	ti.Focus()

	m := &inputModel{ti: ti}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr), tea.WithInput(os.Stdin))
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	fm, ok := final.(*inputModel)
	if !ok || fm.canceled {
		return "", ErrCanceled
	}
	return strings.TrimRight(fm.ti.Value(), " "), nil
}

func hintFor(prompt string) string {
	if prompt == "" {
		prompt = "input"
	}
	return "Enter: " + prompt + ", Ctrl+C: cancel"
}

type inputModel struct {
	ti       textinput.Model
	canceled bool
	done     bool
}

func (m *inputModel) Init() tea.Cmd { return textinput.Blink }

func (m *inputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "ctrl+c", "esc":
			m.canceled = true
			return m, tea.Quit
		case "enter":
			m.done = true
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	return m, cmd
}

func (m *inputModel) View() string {
	if m.done || m.canceled {
		return ""
	}
	return m.ti.View() + "\n"
}
