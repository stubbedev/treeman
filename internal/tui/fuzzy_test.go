package tui

import (
	"strings"
	"testing"
)

func find(t *testing.T, value, query string) fuzzyMatch {
	t.Helper()
	fm, ok := fuzzyFind([]rune(value), []rune(query))
	if !ok {
		t.Fatalf("fuzzyFind(%q, %q): expected match", value, query)
	}
	return fm
}

func miss(t *testing.T, value, query string) {
	t.Helper()
	if fm, ok := fuzzyFind([]rune(value), []rune(query)); ok {
		t.Fatalf("fuzzyFind(%q, %q): expected miss, got %+v", value, query, fm)
	}
}

func TestFuzzySubsequenceMatches(t *testing.T) {
	// Long structured branch names: non-adjacent runes must match.
	branch := "feature/KON-13126-keep-token-in-user-register-flow"
	for _, q := range []string{"ktoken", "13126reg", "fku", "KON"} {
		find(t, branch, q)
	}
	// Order still matters — a reversed subsequence is not one.
	miss(t, branch, "reg13126")
	miss(t, "abc", "abcd")
}

func TestFuzzySmartCase(t *testing.T) {
	// All-lowercase query folds both sides.
	find(t, "Feature/KON-Keep", "kon")
	// Any uppercase makes the whole match case-sensitive.
	miss(t, "feature/kon-keep", "KON")
	find(t, "feature/KON-keep", "KON")
}

func TestFuzzyPositionsAscendingAndComplete(t *testing.T) {
	fm := find(t, "axbxc", "abc")
	if len(fm.positions) != 3 {
		t.Fatalf("positions = %v, want 3", fm.positions)
	}
	for i := 1; i < len(fm.positions); i++ {
		if fm.positions[i] <= fm.positions[i-1] {
			t.Fatalf("positions not ascending: %v", fm.positions)
		}
	}
	for _, p := range fm.positions {
		if p < 0 || p >= len("axbxc") {
			t.Fatalf("position %d out of range", p)
		}
	}
}

// The fzf scoring criteria, from its algorithm docs: prefer matches at
// boundaries, weight the first pattern rune heavier, and prefer
// consecutive runs.
func TestFuzzyRankingCriteria(t *testing.T) {
	// fzf's own example: "fo-bar" beats "foob-r" on "br" — the b sits
	// after a boundary.
	foBar := find(t, "fo-bar", "br")
	foobR := find(t, "foob-r", "br")
	if foBar.score <= foobR.score {
		t.Errorf("fo-bar (%d) should outrank foob-r (%d)", foBar.score, foobR.score)
	}

	// Consecutive beats gapped: "foobar" vs "f.o.o.b.a.r" on "foob".
	tight := find(t, "foobar", "foob")
	gapped := find(t, "f.o.o.b.a.r", "foob")
	if tight.score <= gapped.score {
		t.Errorf("consecutive (%d) should outrank gapped (%d)", tight.score, gapped.score)
	}

	// Prefix/boundary beats buried: exact word-start match of the whole
	// query is the best outcome.
	srcUser := find(t, "src/app/models/user.go", "user")
	bareUser := find(t, "user.go", "user")
	if bareUser.score <= srcUser.score {
		t.Errorf("user.go (%d) should outrank src/.../user.go (%d)", bareUser.score, srcUser.score)
	}
}

func TestFuzzyEmptyQuery(t *testing.T) {
	fm, ok := fuzzyFind([]rune("anything"), nil)
	if !ok || fm.score != 0 || fm.positions != nil {
		t.Fatalf("empty query should be a zero match, got %+v ok=%v", fm, ok)
	}
}

func TestRefilterRanksAndKeepsOriginalIndices(t *testing.T) {
	m := newModel([]string{"src/app/models/user.go", "user.go", "unrelated"}, Options{Height: 10})
	m.input.SetValue("usergo")
	m.refilter()
	want := []int{1, 0} // user.go first, then the buried path; unrelated drops
	if len(m.filtered) != len(want) {
		t.Fatalf("filtered = %v, want %v", m.filtered, want)
	}
	for i, w := range want {
		if m.filtered[i] != w {
			t.Fatalf("filtered = %v, want %v (best match first)", m.filtered, want)
		}
	}
	if got := m.matched[1]; len(got) != len("usergo") {
		t.Errorf("matched positions for user.go = %v, want 6 entries", got)
	}
}

func TestRefilterEmptyQueryKeepsOrder(t *testing.T) {
	items := []string{"c", "a", "b"}
	m := newModel(items, Options{Height: 10})
	if len(m.filtered) != 3 {
		t.Fatalf("filtered = %v", m.filtered)
	}
	for i, want := range []int{0, 1, 2} {
		if m.filtered[i] != want {
			t.Fatalf("empty query must keep input order: %v", m.filtered)
		}
	}
}

func TestRefilterMatchesValuesNotDisplayRows(t *testing.T) {
	items := []string{
		"[worktree] feature/one  \x1b[2m→ /repo/.worktrees/one\x1b[0m",
		"[branch]   feature/two",
	}
	m := newModel(items, Options{Height: 10, Values: []string{"feature/one", "feature/two"}})
	m.input.SetValue("two")
	m.refilter()
	if len(m.filtered) != 1 || m.filtered[0] != 1 {
		t.Fatalf("filtered = %v, want [1] (matched via Values)", m.filtered)
	}
}

func TestHighlightMatchUnderlinesEveryMatchedRune(t *testing.T) {
	// "abc" over "a-xbxc" matches a, b, c at 0, 2, 4 — three separate
	// underline runs, merged where adjacent.
	m := newModel([]string{"a-xbxc"}, Options{Height: 10})
	m.input.SetValue("abc")
	m.refilter()
	if len(m.filtered) != 1 {
		t.Fatal("expected match")
	}
	pos := m.matched[0]
	if len(pos) != 3 {
		t.Fatalf("positions = %v, want 3", pos)
	}
	got := underlinePositions("a-xbxc", pos)
	underlines := strings.Count(got, "\x1b[4m")
	if underlines != 3 {
		t.Errorf("highlight = %q, want 3 underline starts, got %d", got, underlines)
	}
	if strings.Count(got, "\x1b[24m") != 3 {
		t.Errorf("highlight = %q, want 3 underline ends", got)
	}
	if !strings.HasPrefix(got, "\x1b[4ma\x1b[24m") {
		t.Errorf("highlight = %q, first rune a should be underlined", got)
	}
	merged := underlinePositions("abc", []int{0, 1})
	// Adjacent offsets merge into one run.
	if merged != "\x1b[4mab\x1b[24mc" {
		t.Errorf("merged = %q", merged)
	}
}

func TestValuesLengthMismatchRejected(t *testing.T) {
	_, err := Select([]string{"a", "b"}, Options{Values: []string{"only-a"}})
	if err == nil || !strings.Contains(err.Error(), "1:1") {
		t.Fatalf("err = %v, want Values/items length mismatch", err)
	}
}
