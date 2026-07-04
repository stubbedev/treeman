// Package gitx holds the small, pure git-workflow helpers that back
// the `treeman git` subcommands: ticket-prefix commit messages, the
// protected-branch push guard, branch-name slugification, patch
// filenames, and `git status --porcelain` parsing for the interactive
// stager. Everything here is deterministic and side-effect free so it
// can be unit-tested without a repo or a TTY — the git subprocess and
// the TUI live in the command layer.
package gitx

import (
	"path"
	"regexp"
	"strings"
)

// ticketRe matches a Jira-style key anywhere in a branch name, e.g.
// `feature/KON-1234-foo` → `KON-1234`.
var ticketRe = regexp.MustCompile(`[A-Z]+-[0-9]+`)

// explicitPrefixRe matches a message the user already prefixed with a
// ticket key (`KON-1234: ...`), so we don't double-prefix it.
var explicitPrefixRe = regexp.MustCompile(`^[A-Z]+-[0-9]+:`)

// TicketPrefix returns the first `ABC-123` ticket key embedded in
// `branch`, or "" when there is none.
func TicketPrefix(branch string) string {
	return ticketRe.FindString(branch)
}

// ApplyTicketPrefix prepends `prefix: ` to `msg` unless prefix is
// empty or the message already carries its own explicit ticket
// prefix. Mirrors the zsh `gcm` auto-prefix.
func ApplyTicketPrefix(msg, prefix string) string {
	if prefix == "" || explicitPrefixRe.MatchString(msg) {
		return msg
	}
	return prefix + ": " + msg
}

// protectedExact is the set of branch names that always warn on push.
var protectedExact = map[string]struct{}{
	"develop": {}, "staging": {}, "production": {}, "master": {}, "main": {},
}

// protectedGlobs are the always-on patterns; callers append repo
// `gp.protected` globs.
var protectedGlobs = []string{"release/*", "hotfix/*"}

// ProtectedPush reports why pushing `branch` should prompt for
// confirmation, or "" when the branch is unprotected. `extraGlobs`
// come from `git config --get-all gp.protected`.
func ProtectedPush(branch string, extraGlobs []string) string {
	if _, ok := protectedExact[branch]; ok {
		return "protected branch '" + branch + "'"
	}
	for _, pat := range append(protectedGlobs, extraGlobs...) {
		if pat == "" {
			continue
		}
		// path.Match treats `/` specially, which is exactly what we
		// want for `release/*` (matches `release/1.2.3`, not nested).
		if ok, _ := path.Match(pat, branch); ok {
			return "branch '" + branch + "' matches '" + pat + "'"
		}
	}
	return ""
}

// handleKeyRe splits a pasted ticket reference from its description:
// leading letters + digits (any -/_/space separator between and
// after), the digit run required to end on a separator or end-of-
// string so `add2fa` isn't misread as a key. Group 3 is the trailing
// description (empty when there's just a key).
var handleKeyRe = regexp.MustCompile(`^([A-Za-z]+)[-_ ]*([0-9]+)([-_ ].*|)$`)

// nonSlug collapses runs of non-`[a-z0-9]` into a single dash.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Handleize turns arbitrary pasted text into a git-ref-safe handle.
// A leading ticket key (letters+digits) is upper-cased and dashed to
// `KON-1234` so `gcm`'s commit-prefix keeps matching; the remainder is
// lowercased with non-alnum runs collapsed to dashes and edges
// trimmed.
//
//	"KON 1234 Fix the login" → "KON-1234-fix-the-login"
//	"add2fa"                 → "add2fa"  (not a key)
func Handleize(text string) string {
	v := strings.TrimSpace(text)
	var key string
	if m := handleKeyRe.FindStringSubmatch(v); m != nil {
		key = strings.ToUpper(m[1]) + "-" + m[2]
		v = m[3] // remainder (may be empty), leading separator trimmed by slug()
	}
	slug := slugify(v)
	switch {
	case key != "" && slug != "":
		return key + "-" + slug
	case key != "":
		return key
	default:
		return slug
	}
}

// slugify lowercases, collapses non-alnum to dashes, trims edge dashes.
func slugify(v string) string {
	v = strings.ToLower(v)
	v = nonSlug.ReplaceAllString(v, "-")
	return strings.Trim(v, "-")
}

// PatchFilename derives the `<cur>--<target>.diff` filename for a
// three-dot branch diff export (zsh `gcd`). `remotes/origin/` prefixes
// are stripped and slashes flattened to dashes so the name is
// filesystem-safe.
func PatchFilename(cur, target string) string {
	return cleanRef(cur) + "--" + cleanRef(target) + ".diff"
}

func cleanRef(ref string) string {
	ref = strings.TrimPrefix(ref, "remotes/origin/")
	return strings.ReplaceAll(ref, "/", "-")
}

// StageKind is the type-aware action for a StageRow. Its value is the
// status letter (M/D/A) so String renders it directly.
type StageKind rune

// String renders the one-letter status marker (e.g. "M").
func (k StageKind) String() string { return string(rune(k)) }

const (
	// StageModified — a worktree-modified tracked file (`git add -p`).
	StageModified StageKind = 'M'
	// StageDeleted — a worktree deletion to stage (`git add`).
	StageDeleted StageKind = 'D'
	// StageAdded — an untracked path to stage (`git add`).
	StageAdded StageKind = 'A'
)

// StageRow is one candidate line for the interactive stager.
type StageRow struct {
	Kind StageKind
	Path string
	// ExpandDir is true for an untracked directory (`?? dir/`) whose
	// contained files the caller should walk and stage individually.
	ExpandDir bool
}

// StageRows parses `git status --porcelain` output into the M/A/D rows
// the interactive stager offers. It looks only at the worktree
// (second) status column — staged-only changes are already staged and
// so are skipped — plus untracked entries. Directory expansion for
// untracked dirs is left to the caller (see ExpandDir) to keep this
// filesystem-free and testable.
func StageRows(porcelain string) []StageRow {
	var rows []StageRow
	for line := range strings.SplitSeq(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		rest := line[3:]
		switch {
		case xy == "??":
			rows = append(rows, StageRow{Kind: StageAdded, Path: rest, ExpandDir: strings.HasSuffix(rest, "/")})
		case xy[1] == 'D':
			rows = append(rows, StageRow{Kind: StageDeleted, Path: rest})
		case xy[1] == 'M':
			p := rest
			if i := strings.Index(p, " -> "); i >= 0 {
				p = p[i+4:] // rename: keep the new name
			}
			rows = append(rows, StageRow{Kind: StageModified, Path: p})
		}
	}
	return rows
}
