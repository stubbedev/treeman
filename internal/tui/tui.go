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
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/stubbedev/treeman/internal/ui"
)

// ErrCanceled is returned when the user cancels a picker step with
// Ctrl+C. Callers treat it as "no selection" — the same as fzf's
// non-zero exit — and may route it somewhere (e.g. the branch wizard).
var ErrCanceled = errors.New("tui: canceled")

// ErrAborted is returned when the user hits Esc: quit the WHOLE
// command, no action — never just the current step. It wraps
// ErrCanceled so existing `errors.Is(err, ErrCanceled)` quit-paths
// catch it; flows that give Ctrl+C a step-level meaning must check
// ErrAborted first and bail out entirely.
var ErrAborted = fmt.Errorf("esc: %w", ErrCanceled)

// ErrNotTTY is returned when there's no terminal to drive the picker.
// Callers fall back to non-interactive behaviour or abort with a hint.
var ErrNotTTY = errors.New("interactive picker needs a terminal (pass arguments instead when scripting)")

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
	Prompt     string   // what Enter does, e.g. "switch/create" (footer hint)
	Query      string   // initial filter text
	Height     int      // max visible rows (0 → $TREEMAN_PICKER_HEIGHT, else 10)
	Header     string   // static header line shown under the prompt
	Multi      bool     // Tab-toggle multi-select
	Actions    []Action // extra key bindings
	CancelHint string   // what Ctrl+C does (footer hint; default "cancel")
	// Values optionally carries the RAW string each item stands for
	// when the display rows are decorated (ANSI, prefixes, markers):
	// the fuzzy filter and ranking run against these, not the display
	// text. Nil → items are matched as-is. Must match items 1:1.
	Values []string
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

// pickerHeight resolves the default list height: the
// $TREEMAN_PICKER_HEIGHT override when set to a sane integer, else 10.
func pickerHeight() int {
	if v := os.Getenv("TREEMAN_PICKER_HEIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 2 && n <= 100 {
			return n
		}
	}
	return defaultHeight
}

// Interactive reports whether a picker can run: stdin readable as a
// TTY (for keys) and stderr a TTY (for rendering). Commands use it to
// pick a non-interactive degradation up front (e.g. `git commit` with
// the message passed as args).
func Interactive() bool {
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
	if len(opts.Values) != 0 && len(opts.Values) != len(items) {
		return Result{Canceled: true}, fmt.Errorf("tui: Values (%d) must match items (%d) 1:1", len(opts.Values), len(items))
	}
	if !Interactive() {
		return Result{Canceled: true}, ErrNotTTY
	}
	// Styling keys off stdout by default, but the picker renders to
	// stderr — and stdout is often a $() capture (the cd shims).
	ui.EnableColorForStderr()
	if opts.Height <= 0 {
		opts.Height = pickerHeight()
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
	if fm.aborted {
		return Result{Query: fm.input.Value(), Canceled: true}, ErrAborted
	}
	if fm.canceled {
		return Result{Query: fm.input.Value(), Canceled: true}, ErrCanceled
	}
	return fm.result(), nil
}

// model is the shared filterable-list state. Query editing lives in
// the embedded textinput — one implementation of caret movement,
// word/backward deletes, and paste handling, shared with the plain
// input prompt — so the picker gains readline-style editing for free.
type model struct {
	items    []string
	opts     Options
	input    textinput.Model
	filtered []int         // indices into items, post-filter
	cursor   int           // index into filtered
	marked   map[int]bool  // keyed by ORIGINAL item index
	matched  map[int][]int // original index → rune offsets of the matched query runes
	action   string
	canceled bool // Ctrl+C — cancel this step
	aborted  bool // Esc — quit the whole command
	quit     bool
}

func newModel(items []string, opts Options) *model {
	ti := textinput.New()
	ti.SetValue(opts.Query)
	ti.CursorEnd()
	ti.Focus()
	// ponytail: bubbles' own reverse-video blink cursor is hidden here —
	// View() draws the same static "▌" half-block cursor as the plain
	// input prompt.
	ti.Cursor.SetMode(cursor.CursorHide)
	m := &model{items: items, opts: opts, input: ti, marked: map[int]bool{}}
	m.refilter()
	return m
}

func (m *model) Init() tea.Cmd { return nil }

// refilter recomputes the visible subset from the current query and
// clamps the cursor. Queries are matched fuzzily (smart-case
// subsequence, see fuzzy.go) against each item's VALUE — opts.Values
// when the display rows are decorated — and the survivors are ranked
// best-match-first. An empty query keeps every item in input order.
func (m *model) refilter() {
	m.filtered = m.filtered[:0]
	m.matched = nil
	if m.input.Value() == "" {
		for i := range m.items {
			m.filtered = append(m.filtered, i)
		}
	} else {
		query := []rune(m.input.Value())
		type hit struct {
			idx int
			fm  fuzzyMatch
		}
		var hits []hit
		for i, v := range m.values() {
			if fm, ok := fuzzyFind([]rune(v), query); ok {
				hits = append(hits, hit{i, fm})
			}
		}
		sort.SliceStable(hits, func(a, b int) bool {
			return hits[a].fm.score > hits[b].fm.score
		})
		m.matched = map[int][]int{}
		for _, h := range hits {
			m.filtered = append(m.filtered, h.idx)
			m.matched[h.idx] = h.fm.positions
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// values returns the strings the filter matches against: opts.Values
// when given, else the display items themselves.
func (m *model) values() []string {
	if len(m.opts.Values) != 0 {
		return m.opts.Values
	}
	return m.items
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

// handleKey applies list-navigation keys itself and routes everything
// else (runes, backspace/delete, caret movement, ctrl+w/u, paste…)
// through the textinput. List nav is dispatched first so the keys the
// list owns never reach the editor.
func (m *model) handleKey(key tea.KeyMsg) tea.Cmd {
	switch key.String() {
	case "esc":
		m.aborted = true
		return tea.Quit
	case "ctrl+c":
		m.canceled = true
		return tea.Quit
	case "enter":
		m.quit = true
		return tea.Quit
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
		return nil
	case "down", "ctrl+n":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
		}
		return nil
	case "pgup":
		m.cursor = max(m.cursor-m.opts.Height, 0)
		return nil
	case "pgdown":
		m.cursor = min(m.cursor+m.opts.Height, max(len(m.filtered)-1, 0))
		return nil
	case "tab":
		m.markCursor()
		return nil
	case "shift+tab":
		if m.opts.Multi {
			m.toggleAll()
		}
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(key)
	m.refilter()
	return cmd
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
// marked set in multi mode (falling back to the highlighted row when
// nothing is marked — fzf's Enter behaviour), else the single
// highlighted item.
func (m *model) selection() []int {
	if len(m.filtered) == 0 {
		return nil
	}
	if m.opts.Multi {
		var out []int
		for _, i := range m.filtered { // stable, filtered order
			if m.marked[i] {
				out = append(out, i)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []int{m.filtered[m.cursor]}
}

// markedCount returns how many items are currently marked.
func (m *model) markedCount() int {
	n := 0
	for _, on := range m.marked {
		if on {
			n++
		}
	}
	return n
}

func (m *model) result() Result {
	sel := m.selection()
	idx := -1
	if len(m.filtered) > 0 {
		idx = m.filtered[m.cursor]
	}
	return Result{Index: idx, Indices: sel, Query: m.input.Value(), Action: m.action}
}

// View renders:
//
//	❯ query▌                       12/45
//	  <optional dim header>
//	  ↑ 3 more
//	❯ ● item (cursor row, bold, match highlighted)
//	  ○ item
//	  ↓ 12 more
//	  enter switch · tab mark · ctrl+c cancel
func (m *model) View() string {
	if m.quit || m.canceled || m.aborted {
		return "" // clear the inline picker on exit
	}
	var b strings.Builder

	// Prompt line: pointer + live query with the block caret at its
	// position + match count.
	b.WriteString(ui.Cyan(ui.SymPointer + " "))
	value := []rune(m.input.Value())
	pos := min(m.input.Position(), len(value))
	b.WriteString(string(value[:pos]))
	b.WriteString(ui.Dim("▌"))
	b.WriteString(string(value[pos:]))
	b.WriteString(ui.Dim(fmt.Sprintf("  %d/%d", len(m.filtered), len(m.items))))
	b.WriteString("\n")

	if m.opts.Header != "" {
		b.WriteString("  " + ui.Dim(m.opts.Header) + "\n")
	}

	if len(m.filtered) == 0 {
		b.WriteString("  " + ui.Dim("no matches") + "\n")
		b.WriteString(m.footer())
		return b.String()
	}

	win := m.visible()
	if first := win[0]; first > 0 {
		b.WriteString("  " + ui.Dim(fmt.Sprintf("↑ %d more", first)) + "\n")
	}
	for _, i := range win {
		orig := m.filtered[i]
		cursor := i == m.cursor
		if cursor {
			b.WriteString(ui.Cyan(ui.SymPointer + " "))
		} else {
			b.WriteString("  ")
		}
		if m.opts.Multi {
			if m.marked[orig] {
				b.WriteString(ui.Green(ui.SymMarkOn) + " ")
			} else {
				b.WriteString(ui.Dim(ui.SymMarkOff) + " ")
			}
		}
		line := m.highlightMatch(orig)
		if cursor {
			line = ui.Bold(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	if last := win[len(win)-1]; last < len(m.filtered)-1 {
		b.WriteString("  " + ui.Dim(fmt.Sprintf("↓ %d more", len(m.filtered)-1-last)) + "\n")
	}
	b.WriteString(m.footer())
	return b.String()
}

// highlightMatch underlines every rune the fuzzy matcher paired
// with a query rune (positions from the scorer, so non-adjacent
// matches highlight too). Items that already carry ANSI styling
// (pre-colored menu rows) are returned untouched — splicing codes
// into them would garble the escapes.
func (m *model) highlightMatch(orig int) string {
	item := m.items[orig]
	positions := m.matched[orig]
	if m.input.Value() == "" || positions == nil || !ui.ColorEnabled() || strings.ContainsRune(item, 0x1b) {
		return item
	}
	return underlinePositions(m.values()[orig], positions)
}

// underlinePositions wraps the rune at each listed offset in
// underline on/off escapes, merging adjacent offsets into runs.
func underlinePositions(value string, positions []int) string {
	var b strings.Builder
	underlined := false
	for i, r := range value {
		on := slices.Contains(positions, i)
		if on != underlined {
			if on {
				b.WriteString("\x1b[4m")
			} else {
				b.WriteString("\x1b[24m")
			}
			underlined = on
		}
		b.WriteRune(r)
	}
	if underlined {
		b.WriteString("\x1b[24m")
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

// footer renders the dim key-hint line: what Enter/Tab/Ctrl+C do, any
// action hints, and the marked count in multi mode.
func (m *model) footer() string {
	sep := " " + ui.SymDot + " "
	enter := m.opts.Prompt
	if enter == "" {
		enter = "select"
	}
	parts := []string{"enter " + enter}
	if m.opts.Multi {
		parts = append(parts, "tab mark")
	}
	for _, a := range m.opts.Actions {
		if a.Hint != "" {
			parts = append(parts, a.Hint)
		}
	}
	// Esc always quits the whole command. When Ctrl+C means the same
	// thing one combined hint suffices; when it has a step meaning
	// (e.g. "wizard") spell both out.
	if cancel := m.opts.CancelHint; cancel != "" && cancel != "cancel" {
		parts = append(parts, "ctrl+c "+cancel, "esc quit")
	} else {
		parts = append(parts, "esc cancel")
	}
	line := "  " + ui.Dim(strings.Join(parts, sep))
	if n := m.markedCount(); n > 0 {
		line += ui.Green(fmt.Sprintf("%s%d marked", sep, n))
	}
	return line + "\n"
}
