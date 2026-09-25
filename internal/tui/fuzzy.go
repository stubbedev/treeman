// fuzzy.go — a faithful port of fzf's FuzzyMatchV2 (junegunn/fzf,
// src/algo/algo.go, default scheme as of 0.73.x), vendored rather than
// pulling a dependency. Matches are subsequence alignments found with a
// modified Smith-Waterman DP, scored with boundary/camel-case/consecutive
// bonuses and affine gap penalties, so the best match floats to the top
// of the picker instead of keeping input order. Oversized inputs fall
// back to fzf's greedy V1 exactly like upstream does.
//
// Differences from upstream, all behaviour-neutral for our inputs:
//   - no slab/bytes fast paths (picker items are short) and no rune
//     normalization,
//   - always forward, always with positions,
//   - int instead of int16 score cells,
//   - positions returned ascending.
package tui

import (
	"slices"
	"strings"
	"unicode"
)

// charClass classifies a rune for boundary-bonus purposes.
type charClass int

const (
	charWhite charClass = iota
	charNonWord
	charDelimiter
	charLower
	charUpper
	charLetter
	charNumber
)

const charClassCount = int(charNumber) + 1

const (
	scoreMatch        = 16
	scoreGapStart     = -3
	scoreGapExtension = -1

	// We prefer matches at the beginning of a word, but the bonus should
	// not be too great to prevent the longer acronym matches from always
	// winning over shorter fuzzy matches. The bonus point here was
	// specifically chosen that the bonus is cancelled when the gap between
	// the acronyms grows over 8 characters.
	bonusBoundary = scoreMatch / 2

	// Although bonus point for non-word characters is non-contextual, we
	// need it for computing bonus points for consecutive chunks starting
	// with a non-word character.
	bonusNonWord = scoreMatch / 2

	// Edge-triggered bonus for matches in camelCase words.
	// Compared to word-boundary case, they don't accompany single-character
	// gaps (e.g. FooBar vs. foo-bar), so we deduct bonus point accordingly.
	bonusCamel123 = bonusBoundary + scoreGapExtension

	// Minimum bonus point given to characters in consecutive chunks.
	bonusConsecutive = -(scoreGapStart + scoreGapExtension)

	// The first character in the typed pattern usually has more
	// significance than the rest so it is important that it appears at
	// special positions where bonus points are given, e.g. "to-go" vs.
	// "ongoing" on "og" or on "ogo". The amount of the extra bonus should
	// be limited so that the gap penalty is still respected.
	bonusFirstCharMultiplier = 2
)

var (
	// Extra bonus for word boundary after whitespace or beginning of
	// string (default scheme).
	bonusBoundaryWhite = bonusBoundary + 2

	// Extra bonus for word boundary after a delimiter character.
	bonusBoundaryDelimiter = bonusBoundary + 1

	// The char class before the start of the string (default scheme).
	initialCharClass = charWhite

	whiteChars     = " \t\n\v\f\r\x85\xA0"
	delimiterChars = "/,:;|"

	asciiCharClasses [unicode.MaxASCII + 1]charClass
	bonusMatrix      [charClassCount][charClassCount]int
)

// init mirrors fzf's algo.Init("default"): precompute the ASCII class
// table and the class-transition bonus matrix.
func init() {
	for i := 0; i <= unicode.MaxASCII; i++ {
		char := rune(i)
		c := charNonWord
		switch {
		case char >= 'a' && char <= 'z':
			c = charLower
		case char >= 'A' && char <= 'Z':
			c = charUpper
		case char >= '0' && char <= '9':
			c = charNumber
		case strings.ContainsRune(whiteChars, char):
			c = charWhite
		case strings.ContainsRune(delimiterChars, char):
			c = charDelimiter
		}
		asciiCharClasses[i] = c
	}
	for i := range charClassCount {
		for j := range charClassCount {
			bonusMatrix[i][j] = bonusFor(charClass(i), charClass(j))
		}
	}
}

func charClassOfNonASCII(char rune) charClass {
	switch {
	case unicode.IsLower(char):
		return charLower
	case unicode.IsUpper(char):
		return charUpper
	case unicode.IsNumber(char):
		return charNumber
	case unicode.IsLetter(char):
		return charLetter
	case unicode.IsSpace(char):
		return charWhite
	case strings.ContainsRune(delimiterChars, char):
		return charDelimiter
	}
	return charNonWord
}

func charClassOf(char rune) charClass {
	if char <= unicode.MaxASCII {
		return asciiCharClasses[char]
	}
	return charClassOfNonASCII(char)
}

func bonusFor(prevClass, class charClass) int {
	if class >= charNonWord {
		switch prevClass {
		case charWhite:
			// Word boundary after whitespace
			return bonusBoundaryWhite
		case charDelimiter:
			// Word boundary after a delimiter character
			return bonusBoundaryDelimiter
		case charNonWord:
			// Word boundary
			return bonusBoundary
		default:
		}
	}

	if prevClass == charLower && class == charUpper ||
		prevClass != charNumber && class == charNumber {
		// camelCase letter123
		return bonusCamel123
	}

	switch class {
	case charNonWord, charDelimiter:
		return bonusNonWord
	case charWhite:
		return bonusBoundaryWhite
	default:
	}
	return 0
}

// fuzzyMatch is one item's best alignment of the query: the total score
// plus, for highlighting, the ascending rune offsets of every matched
// query rune (len(positions) == len(query runes)).
type fuzzyMatch struct {
	score     int
	positions []int
}

// fuzzyFind returns the best-scoring smart-case subsequence alignment
// of query inside value, or ok=false when there is none.
//
// Smart case: an all-lowercase query matches case-insensitively; any
// uppercase rune in the query turns the whole match case-sensitive.
// An empty query matches with score 0 and no positions.
func fuzzyFind(value, query []rune) (fuzzyMatch, bool) {
	if len(query) == 0 {
		return fuzzyMatch{}, true
	}
	caseSensitive := slices.ContainsFunc(query, unicode.IsUpper)
	if !caseSensitive {
		query = lowerRunes(query)
	}
	return fuzzyMatchV2(caseSensitive, value, query)
}

func lowerRunes(runes []rune) []rune {
	out := make([]rune, len(runes))
	for i, r := range runes {
		out[i] = unicode.ToLower(r)
	}
	return out
}

// maxFuzzyMatrix is the DP size above which we fall back to the greedy
// V1 alignment, mirroring fzf's slab-capacity guard (100*1024 cells).
const maxFuzzyMatrix = 100 * 1024

// fuzzyMatchV2 is the port of fzf's FuzzyMatchV2: a modified
// Smith-Waterman that finds the highest-scoring alignment. Pattern must
// already be lowercase when caseSensitive is false (fzf's contract).
// The phases are split across fuzzyState methods to keep each readable.
func fuzzyMatchV2(caseSensitive bool, value, pattern []rune) (fuzzyMatch, bool) {
	M := len(pattern)
	if M == 0 {
		return fuzzyMatch{}, true
	}
	N := len(value)
	if M > N {
		return fuzzyMatch{}, false
	}
	// O(nm) can be prohibitively expensive for large input; a very long
	// pattern would overflow the score matrix. Fall back like fzf does.
	if N*M > maxFuzzyMatrix || M > 1000 {
		return fuzzyMatchV1(caseSensitive, value, pattern)
	}

	// Phase 1. Narrow the DP window.
	minIdx, maxIdx, ok := fuzzyWindow(value, pattern, caseSensitive)
	if !ok {
		return fuzzyMatch{}, false
	}

	s := newFuzzyState(caseSensitive, value[minIdx:maxIdx], pattern)

	// Phase 2. First-row scores, bonuses, first occurrences.
	if !s.firstRow() {
		return fuzzyMatch{}, false
	}
	if M == 1 {
		return fuzzyMatch{
			score:     s.maxScore,
			positions: []int{minIdx + s.maxScorePos},
		}, true
	}

	// Phase 3. Score matrix. Phase 4. Backtrace.
	s.fillMatrix()
	pos := s.backtrace(minIdx)
	return fuzzyMatch{score: s.maxScore, positions: pos}, true
}

// fuzzyWindow is phase 1: a greedy forward scan narrowing the DP window
// to [minIdx, maxIdx) — the earliest chain of first occurrences, one
// step back before the first match so its true boundary bonus is
// visible, and the last occurrence of the final pattern rune as the
// window's end.
func fuzzyWindow(value, pattern []rune, caseSensitive bool) (minIdx, maxIdx int, ok bool) {
	minIdx, idx, lastIdx := 0, 0, 0
	var last rune
	for pidx := range pattern {
		last = pattern[pidx]
		idx = indexRuneFolded(value, last, idx, caseSensitive)
		if idx < 0 {
			return 0, 0, false
		}
		if pidx == 0 && idx > 0 {
			// Step back to find the right bonus point
			minIdx = idx - 1
		}
		lastIdx = idx
		idx++
	}
	maxIdx = lastIdx + 1
	if lastIdx+1 < len(value) {
		if end := lastIndexRuneFolded(value, last, lastIdx+1, caseSensitive); end >= 0 {
			maxIdx = end + 1
		}
	}
	return minIdx, maxIdx, true
}

// fuzzyState is the shared working set of the V2 phases.
type fuzzyState struct {
	caseSensitive bool
	pattern       []rune
	T             []rune // windowed; case-folded in place by firstRow
	B             []int  // bonus per window offset
	H0, C0        []int  // first-row scores and consecutive-chunk lengths
	F             []int  // first occurrence of each pattern rune
	lastIdx       int    // last window offset of any pattern rune
	maxScore      int
	maxScorePos   int

	// fillMatrix output, shared with backtrace.
	H, C      []int
	f0, width int
}

func newFuzzyState(caseSensitive bool, window, pattern []rune) *fuzzyState {
	return &fuzzyState{
		caseSensitive: caseSensitive,
		pattern:       pattern,
		T:             append([]rune(nil), window...),
		B:             make([]int, len(window)),
		H0:            make([]int, len(window)),
		C0:            make([]int, len(window)),
		F:             make([]int, len(pattern)),
	}
}

// firstRow is phase 2: case-fold the window in place, compute the bonus
// and first-row (H0/C0) scores, and record the first occurrence (F) of
// every pattern rune. Returns false when a pattern rune is missing from
// the window.
func (s *fuzzyState) firstRow() bool {
	M := len(s.pattern)
	pidx := 0
	pchar0, pchar := s.pattern[0], s.pattern[0]
	prevH0, prevClass, inGap := 0, initialCharClass, false
	for off, char := range s.T {
		var class charClass
		if char <= unicode.MaxASCII {
			class = asciiCharClasses[char]
			if !s.caseSensitive && class == charUpper {
				char += 32
				s.T[off] = char
			}
		} else {
			class = charClassOfNonASCII(char)
			if !s.caseSensitive && class == charUpper {
				char = unicode.To(unicode.LowerCase, char)
			}
			s.T[off] = char
		}

		bonus := bonusMatrix[prevClass][class]
		s.B[off] = bonus
		prevClass = class

		if char == pchar {
			if pidx < M {
				s.F[pidx] = off
				pidx++
				pchar = s.pattern[min(pidx, M-1)]
			}
			s.lastIdx = off
		}

		if char == pchar0 {
			score := scoreMatch + bonus*bonusFirstCharMultiplier
			s.H0[off] = score
			s.C0[off] = 1
			if M == 1 && score > s.maxScore {
				s.maxScore, s.maxScorePos = score, off
				if bonus >= bonusBoundary {
					break
				}
			}
			inGap = false
		} else {
			if inGap {
				s.H0[off] = max(prevH0+scoreGapExtension, 0)
			} else {
				s.H0[off] = max(prevH0+scoreGapStart, 0)
			}
			s.C0[off] = 0
			inGap = true
		}
		prevH0 = s.H0[off]
	}
	return pidx == M
}

// fillMatrix is phase 3: fill the score matrix (H) over [f0, lastIdx].
// Unlike the original Smith-Waterman, omission of a pattern rune is not
// allowed. C tracks the possible length of a consecutive chunk at each
// cell. Updates s.maxScore/s.maxScorePos.
func (s *fuzzyState) fillMatrix() {
	M := len(s.pattern)
	s.f0 = s.F[0]
	s.width = s.lastIdx - s.f0 + 1
	s.H = make([]int, s.width*M)
	copy(s.H, s.H0[s.f0:s.lastIdx+1])
	s.C = make([]int, s.width*M)
	copy(s.C, s.C0[s.f0:s.lastIdx+1])

	for pidx := 1; pidx < M; pidx++ {
		f := s.F[pidx]
		pchar := s.pattern[pidx]
		row := pidx * s.width
		// The row's leftmost cell has no left neighbour; zero it so the
		// first gap penalty computes from a clean base (fzf's
		// Hleft[0]=0).
		s.H[row+f-s.f0-1] = 0
		inGap := false
		for off := f; off <= s.lastIdx; off++ {
			inGap = s.fillCell(row, off, pchar, pidx == M-1, inGap)
		}
	}
}

// fillCell computes one matrix cell (pattern rune pchar at window
// offset off of row `row`), stores it into H/C, and reports the new
// inGap state for the row scan.
func (s *fuzzyState) fillCell(row, off int, pchar rune, lastRow, inGap bool) bool {
	col := off
	char := s.T[off]
	var s1, s2, consecutive int

	gap := scoreGapStart
	if inGap {
		gap = scoreGapExtension
	}
	s2 = s.H[row+col-s.f0-1] + gap

	if pchar == char {
		s1 = s.H[row-s.width+col-s.f0-1] + scoreMatch
		b := s.B[col]
		consecutive = s.C[row-s.width+col-s.f0-1] + 1
		if consecutive > 1 {
			fb := s.B[col-consecutive+1]
			// Break consecutive chunk
			if b >= bonusBoundary && b > fb {
				consecutive = 1
			} else {
				b = max(b, bonusConsecutive, fb)
			}
		}
		if s1+b < s2 {
			s1 += s.B[col]
			consecutive = 0
		} else {
			s1 += b
		}
	}
	s.C[row+col-s.f0] = consecutive

	newInGap := s1 < s2
	score := max(s1, s2, 0)
	if lastRow && score > s.maxScore {
		s.maxScore, s.maxScorePos = score, col
	}
	s.H[row+col-s.f0] = score
	return newInGap
}

// backtrace is phase 4: recover the matched positions from the best
// final cell, walking the matrix while it dominates its diagonal and
// left neighbours (preferMatch breaks score ties).
func (s *fuzzyState) backtrace(minIdx int) []int {
	M := len(s.pattern)
	pos := make([]int, 0, M)
	i := M - 1
	j := s.maxScorePos
	preferMatch := true
	for {
		row := i
		I := i * s.width
		j0 := j - s.f0
		score := s.H[I+j0]

		var s1, s2 int
		if i > 0 && j >= s.F[i] {
			s1 = s.H[I-s.width+j0-1]
		}
		if j > s.F[i] {
			s2 = s.H[I+j0-1]
		}

		if score > s1 && (score > s2 || score == s2 && preferMatch) {
			pos = append(pos, j+minIdx)
			if i == 0 {
				break
			}
			i--
		}
		preferMatch = s.C[I+j0] > 1 ||
			row+1 < M && j < s.lastIdx && j+1 >= s.F[row+1] && s.C[I+s.width+j0+1] > 0
		j--
	}
	// Backtrace yields last-to-first; flip to ascending.
	slices.Reverse(pos)
	return pos
}

// fuzzyMatchV1 is the port of fzf's FuzzyMatchV1 greedy fallback: find
// the first fuzzy occurrence, trim it backwards, score the resulting
// span. Not guaranteed to find the highest-scoring alignment.
func fuzzyMatchV1(caseSensitive bool, value, pattern []rune) (fuzzyMatch, bool) {
	M := len(pattern)
	N := len(value)
	pidx, sidx, eidx := 0, -1, -1
	for index := range N {
		char := foldRune(value[index], caseSensitive)
		pchar := pattern[pidx]
		if char == pchar {
			if sidx < 0 {
				sidx = index
			}
			if pidx++; pidx == M {
				eidx = index + 1
				break
			}
		}
	}
	if sidx < 0 || eidx < 0 {
		return fuzzyMatch{}, false
	}
	pidx--
	for index := eidx - 1; index >= sidx; index-- {
		char := foldRune(value[index], caseSensitive)
		pchar := pattern[pidx]
		if char == pchar {
			if pidx--; pidx < 0 {
				sidx = index
				break
			}
		}
	}
	score, pos := fuzzyCalculateScore(caseSensitive, value, pattern, sidx, eidx)
	return fuzzyMatch{score: score, positions: pos}, true
}

func foldRune(char rune, caseSensitive bool) rune {
	if caseSensitive {
		return char
	}
	if char >= 'A' && char <= 'Z' {
		return char + 32
	}
	if char > unicode.MaxASCII {
		return unicode.To(unicode.LowerCase, char)
	}
	return char
}

// fuzzyCalculateScore ports fzf's calculateScore: score the fixed span
// [sidx, eidx) against the pattern with the same criteria as V2.
func fuzzyCalculateScore(caseSensitive bool, value, pattern []rune, sidx, eidx int) (int, []int) {
	pidx, score, inGap, consecutive, firstBonus := 0, 0, false, 0, 0
	pos := make([]int, 0, len(pattern))
	prevClass := initialCharClass
	if sidx > 0 {
		prevClass = charClassOf(value[sidx-1])
	}
	for idx := sidx; idx < eidx; idx++ {
		char := value[idx]
		class := charClassOf(char)
		if foldRune(char, caseSensitive) == pattern[pidx] {
			pos = append(pos, idx)
			var b int
			b, consecutive, firstBonus = matchBonus(prevClass, class, consecutive, firstBonus)
			score += scoreMatch
			if pidx == 0 {
				score += b * bonusFirstCharMultiplier
			} else {
				score += b
			}
			inGap = false
			pidx++
		} else {
			if inGap {
				score += scoreGapExtension
			} else {
				score += scoreGapStart
			}
			inGap = true
			consecutive = 0
			firstBonus = 0
		}
		prevClass = class
	}
	return score, pos
}

// matchBonus scores a matched rune inside a consecutive chunk: the
// first rune of a chunk carries its boundary bonus, later runes get at
// least bonusConsecutive (or the chunk's first bonus when the run
// started at a stronger boundary).
func matchBonus(prevClass, class charClass, consecutive, firstBonus int) (bonus, newConsecutive, newFirstBonus int) {
	bonus = bonusMatrix[prevClass][class]
	if consecutive == 0 {
		return bonus, 1, bonus
	}
	if bonus >= bonusBoundary && bonus > firstBonus {
		firstBonus = bonus
	}
	return max(bonus, firstBonus, bonusConsecutive), consecutive + 1, firstBonus
}

func indexRuneFolded(runes []rune, target rune, from int, caseSensitive bool) int {
	for i := from; i < len(runes); i++ {
		if runeEqFolded(runes[i], target, caseSensitive) {
			return i
		}
	}
	return -1
}

func lastIndexRuneFolded(runes []rune, target rune, from int, caseSensitive bool) int {
	for i := len(runes) - 1; i >= from; i-- {
		if runeEqFolded(runes[i], target, caseSensitive) {
			return i
		}
	}
	return -1
}

// runeEqFolded compares with the same folding phase 2 applies, so the
// phase-1 window can never exclude a match phase 2 would find.
func runeEqFolded(r, target rune, caseSensitive bool) bool {
	return foldRune(r, caseSensitive) == target
}
