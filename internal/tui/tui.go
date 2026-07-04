// Package tui provides the native interactive pickers behind the
// `treeman git` / `treeman wt` commands — a filterable single/multi
// select and a text input — replacing the external fzf the shell
// wrappers used to shell out to.
//
// Rendering goes to stderr and key input is read from the TTY, so a
// command can drive a picker while still printing its real result
// (e.g. a worktree path for `cd "$(treeman worktree switch …)"`) cleanly on
// stdout — exactly how fzf behaves.
package tui

import (
	"errors"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/stubbedev/treeman/internal/ui"
)

// ErrCanceled is returned when the user aborts a picker (Ctrl+C / Esc).
// Callers treat it as "no selection" — the same as fzf's non-zero exit.
var ErrCanceled = errors.New("tui: canceled")

// ErrNotTTY is returned when there's no terminal to drive the picker.
// Callers fall back to non-interactive behaviour or abort with a hint.
var ErrNotTTY = errors.New("tui: not a terminal")

// Action is an extra key binding on a picker (e.g. Ctrl+X in the log
// view). When its key is pressed the picker exits with Result.Action
// set to Name.
type Action struct {
	Key            string // bubbletea key string, e.g. "ctrl+x"
	Name           string // echoed back in Result.Action
	Hint           string // shown in the prompt hint line
	NeedsSelection bool   // ignored when nothing is highlighted/marked
}

// Options configure a picker run.
type Options struct {
	Prompt  string   // hint prefix, e.g. "switch/create"
	Query   string   // initial filter text
	Height  int      // max visible rows (0 → default 10)
	Header  string   // static header line shown under the prompt
	Multi   bool     // Tab-toggle multi-select
	Actions []Action // extra key bindings
}

// Result is the outcome of a picker run.
type Result struct {
	Index    int    // highlighted item's original index, -1 if none
	Indices  []int  // marked set (multi) or [Index] when Index >= 0
	Query    string // final filter text (for "typed-new" detection)
	Action   string // "" for a plain Enter accept, else Action.Name
	Canceled bool
}

const defaultHeight = 10

// interactive reports whether we can drive a picker: stdin readable as
// a TTY (for keys) and stderr a TTY (for rendering).
func interactive() bool {
	return isTTY(os.Stdin) && isTTY(os.Stderr)
}

func isTTY(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Select runs a single-select picker and returns the chosen item's
// original index, the final query text, and any error. Index is -1
// with a nil error when the list was non-empty but nothing matched the
// query (the caller's "typed something new" path).
func Select(items []string, opts Options) (Result, error) {
	opts.Multi = false
	return run(items, opts)
}

// MultiSelect runs a Tab-toggle multi-select picker.
func MultiSelect(items []string, opts Options) (Result, error) {
	opts.Multi = true
	return run(items, opts)
}

// run drives the bubbletea program. UI on stderr, input from stdin.
func run(items []string, opts Options) (Result, error) {
	if !interactive() {
		return Result{Canceled: true}, ErrNotTTY
	}
	if opts.Height <= 0 {
		opts.Height = defaultHeight
	}
	m := newModel(items, opts)
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr), tea.WithInput(os.Stdin))
	final, err := p.Run()
	if err != nil {
		return Result{Canceled: true}, err
	}
	fm, ok := final.(*model)
	if !ok {
		return Result{Canceled: true}, ErrCanceled
	}
	if fm.canceled {
		return Result{Query: fm.query, Canceled: true}, ErrCanceled
	}
	return fm.result(), nil
}

// model is the shared filterable-list state.
type model struct {
	items    []string
	opts     Options
	query    string
	filtered []int        // indices into items, post-filter
	cursor   int          // index into filtered
	marked   map[int]bool // keyed by ORIGINAL item index
	action   string
	canceled bool
	quit     bool
}

func newModel(items []string, opts Options) *model {
	m := &model{items: items, opts: opts, query: opts.Query, marked: map[int]bool{}}
	m.refilter()
	return m
}

func (m *model) Init() tea.Cmd { return nil }

// refilter recomputes the visible subset from the current query and
// clamps the cursor. Substring, case-insensitive, original order.
//
// ponytail: substring filter, not fzf's ranked fuzzy scoring — port
// scoring only if match ordering ever actually matters here.
func (m *model) refilter() {
	q := strings.ToLower(m.query)
	m.filtered = m.filtered[:0]
	for i, it := range m.items {
		if q == "" || strings.Contains(strings.ToLower(it), q) {
			m.filtered = append(m.filtered, i)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if handled, cmd := m.tryAction(key); handled {
		return m, cmd
	}
	return m, m.handleKey(key)
}

// tryAction consumes a user-defined action key. Returns handled=true
// when the key matched one (cmd is tea.Quit unless the action needed a
// selection there wasn't one, in which case it's a no-op).
func (m *model) tryAction(key tea.KeyMsg) (bool, tea.Cmd) {
	for _, a := range m.opts.Actions {
		if key.String() != a.Key {
			continue
		}
		if a.NeedsSelection && len(m.selection()) == 0 {
			return true, nil
		}
		m.action = a.Name
		m.quit = true
		return true, tea.Quit
	}
	return false, nil
}

// handleKey applies a navigation/edit key to the model.
func (m *model) handleKey(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "ctrl+c", "esc":
		m.canceled = true
		return tea.Quit
	case "enter":
		m.quit = true
		return tea.Quit
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "ctrl+n":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
	case "tab":
		m.markCursor()
	case "shift+tab":
		if m.opts.Multi {
			m.toggleAll()
		}
	case "backspace":
		if len(m.query) > 0 {
			r := []rune(m.query)
			m.query = string(r[:len(r)-1])
			m.refilter()
		}
	default:
		if key.Type == tea.KeyRunes && len(key.Runes) > 0 {
			m.query += string(key.Runes)
			m.refilter()
		}
	}
	return nil
}

// markCursor toggles the mark on the highlighted row (multi mode) and
// advances the cursor.
func (m *model) markCursor() {
	if !m.opts.Multi || len(m.filtered) == 0 {
		return
	}
	orig := m.filtered[m.cursor]
	m.marked[orig] = !m.marked[orig]
	if m.cursor < len(m.filtered)-1 {
		m.cursor++
	}
}

// toggleAll marks every currently-filtered item, or clears them all if
// they're already fully marked.
func (m *model) toggleAll() {
	all := true
	for _, i := range m.filtered {
		if !m.marked[i] {
			all = false
			break
		}
	}
	for _, i := range m.filtered {
		m.marked[i] = !all
	}
}

// selection returns the effective selected original indices: the
// marked set in multi mode, else the single highlighted item.
func (m *model) selection() []int {
	if m.opts.Multi {
		var out []int
		for _, i := range m.filtered { // stable, filtered order
			if m.marked[i] {
				out = append(out, i)
			}
		}
		return out
	}
	if len(m.filtered) == 0 {
		return nil
	}
	return []int{m.filtered[m.cursor]}
}

func (m *model) result() Result {
	sel := m.selection()
	idx := -1
	if len(m.filtered) > 0 {
		idx = m.filtered[m.cursor]
	}
	return Result{Index: idx, Indices: sel, Query: m.query, Action: m.action}
}

func (m *model) View() string {
	if m.quit || m.canceled {
		return "" // clear the inline picker on exit
	}
	var b strings.Builder
	b.WriteString(ui.Cyan("(" + m.hints() + ")> "))
	b.WriteString(m.query)
	b.WriteString("\n")
	if m.opts.Header != "" {
		b.WriteString(ui.Dim(m.opts.Header))
		b.WriteString("\n")
	}
	for _, i := range m.visible() {
		orig := m.filtered[i]
		if i == m.cursor {
			b.WriteString(ui.Cyan("> "))
		} else {
			b.WriteString("  ")
		}
		if m.opts.Multi {
			if m.marked[orig] {
				b.WriteString(ui.Green("[x] "))
			} else {
				b.WriteString("[ ] ")
			}
		}
		line := m.items[orig]
		if i == m.cursor {
			line = ui.Bold(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// visible returns the window of filtered-row indices to draw, scrolled
// to keep the cursor on screen and capped at opts.Height.
func (m *model) visible() []int {
	n := len(m.filtered)
	h := m.opts.Height
	if n <= h {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	start := max(m.cursor-h/2, 0)
	if start+h > n {
		start = n - h
	}
	out := make([]int, h)
	for i := range out {
		out[i] = start + i
	}
	return out
}

// hints builds the prompt hint string from the base prompt + actions.
func (m *model) hints() string {
	parts := []string{}
	if m.opts.Prompt != "" {
		parts = append(parts, "Enter: "+m.opts.Prompt)
	} else {
		parts = append(parts, "Enter: select")
	}
	if m.opts.Multi {
		parts = append(parts, "Tab: mark")
	}
	for _, a := range m.opts.Actions {
		if a.Hint != "" {
			parts = append(parts, a.Hint)
		}
	}
	parts = append(parts, "Ctrl+C: cancel")
	return strings.Join(parts, ", ")
}
