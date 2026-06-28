// Package slug derives a worktree slug from the branch name / path,
// and exposes deterministic redis-db indices so worktrees retain
// their per-engine namespace across treeman upgrades.
//
// The SysV cksum reimplementation matches POSIX `cksum` byte-for-
// byte so external shell hooks that compute the same redis indices
// agree with treeman's slugs.
package slug

import (
	"path/filepath"
	"regexp"
	"strings"

	"lukechampine.com/blake3"
)

// Source records where the slug came from.
type Source int

const (
	// SourceTicket — the slug was extracted from a Jira-style ticket
	// pattern (`[A-Z]+-\d+`) in the branch name or directory.
	SourceTicket Source = iota
	// SourcePathHash — fallback when no ticket was found; slug is a
	// stable `wt_<blake3(path)[:8]>`.
	SourcePathHash
	// SourceMain — derived by ForMain() for a repo's main worktree.
	// Always branch-driven (`main_<sanitised_branch>`) so a non-ticket
	// branch like `develop` doesn't collapse to the repo's path hash,
	// which would share a DB across every branch ever checked out at
	// the repo root.
	SourceMain
)

// Slug is the worktree identifier feeding every per-engine template
// (`{slug}`, `{slug_dash}`, `{slug_redis_queue}`, `{slug_redis_cache}`).
type Slug struct {
	Value  string
	Source Source
}

// Dashed returns the slug with `_` replaced by `-`. Used by the
// `{slug_dash}` template key for S3 / minio buckets where
// underscores are invalid.
func (s Slug) Dashed() string {
	return strings.ReplaceAll(s.Value, "_", "-")
}

// RedisIndices returns (queue_db, cache_db), each in 6..15, derived
// deterministically from the slug via POSIX `cksum`.
func (s Slug) RedisIndices() (queueDB, cacheDB uint8) {
	h := sysvCksum([]byte(s.Value))
	queueDB = uint8(h%10 + 6)
	cacheDB = uint8((h/10)%10 + 6)
	return
}

var ticketRe = regexp.MustCompile(`([A-Z]+)-([0-9]+)`)

// For derives a linked worktree's slug purely from its filesystem
// PATH. The `branch` parameter is accepted for signature compatibility
// with the call sites that happen to know the branch, but is
// deliberately IGNORED — folding the branch into the slug is what the
// two failure modes this function guards against both come down to.
//
// Two properties fall out of keying strictly on the canonical path:
//
//   - Branch-stable. The directory is fixed for a worktree's lifetime,
//     so the slug — and with it the worktree's DB names, patched
//     `.env`, ports, redis indices — does NOT churn when the branch
//     swaps underneath it via an in-worktree `git checkout`. This is
//     the foundation `branch_scoped` swapping relies on, and the reason
//     ResolveIdentity has always called us with an empty branch.
//
//   - Collision-free across a shared ticket. Two worktrees whose names
//     embed the SAME Jira ticket (`PROJ-1234` checked out in two
//     different directories, or two branches of one ticket) get
//     DISTINCT slugs, because the path hash always disambiguates. The
//     previous derivation returned a bare `proj_1234` for both and
//     silently overlapped their storage — same DB names, same redis
//     DBs, same patched `.env`.
//
// A ticket-named directory keeps a readable prefix for ergonomics
// (`proj_1234_<pathhash>`); a generic directory is `wt_<pathhash>`. The
// path hash is the sole carrier of uniqueness in both shapes.
func For(worktreePath string, _ string) Slug {
	tag := pathTag(worktreePath)
	if ticket, ok := extractTicket(filepath.Base(worktreePath)); ok {
		return Slug{Value: composeTicket(ticket, tag), Source: SourceTicket}
	}
	return Slug{Value: "wt_" + tag, Source: SourcePathHash}
}

// pathTag is a stable 8-hex-char blake3 digest of the worktree's
// canonical absolute path — the collision-proof component of every
// linked-worktree slug. Falls back to the raw path when Abs fails
// (e.g. the cwd was removed) so a slug is still produced.
func pathTag(worktreePath string) string {
	canonical, err := filepath.Abs(worktreePath)
	if err != nil {
		canonical = worktreePath
	}
	sum := blake3.Sum256([]byte(canonical))
	return hexEncode(sum[:])[:8]
}

// composeTicket joins a readable ticket prefix with the path tag inside
// the 32-char identifier budget every engine's name template must fit.
// Uniqueness lives in the tag, never the prefix, so truncating an
// over-long prefix can't collide two distinct paths — their tags still
// differ.
func composeTicket(ticket, tag string) string {
	out := ticket + "_" + tag
	if len(out) <= 32 {
		return out
	}
	keep := max(32-1-len(tag), 1)
	return ticket[:keep] + "_" + tag
}

// ForMain derives the slug for a repo's main worktree (the repo root
// checkout). Unlike For(), the branch is mandatory and there's no
// path-hash fallback — checking out a non-ticket branch like `develop`
// at the repo root must produce a branch-specific slug (`main_develop`)
// so per-branch DBs in the main wt actually diverge instead of all
// pointing at one shared path-hash slug.
//
// Detached HEAD (empty branch) gets `main_detached_<hex>` keyed off
// the repo path so two detached repos don't collide. Empty branch is
// the edge case at boot before the daemon has detected HEAD.
func ForMain(worktreePath string, branch string) Slug {
	if branch == "" {
		sum := blake3.Sum256([]byte(worktreePath))
		return Slug{Value: "main_detached_" + hexEncode(sum[:])[:8], Source: SourceMain}
	}
	sanitised := sanitiseBranch(branch)
	if sanitised == "" {
		// Branch was symbol-only (`@`, `###`, etc — git allows it).
		// Falling through with an empty sanitised string would produce
		// `main_` which collides for every odd branch in this repo;
		// hash the raw branch instead so each one gets a stable slug.
		sum := blake3.Sum256([]byte(branch))
		return Slug{Value: "main_sym_" + hexEncode(sum[:])[:8], Source: SourceMain}
	}
	v := "main_" + sanitised
	if len(v) > 32 {
		// Same naive-truncation trap as extractTicket: collide-prone
		// without a hash suffix when the branch name is long.
		sum := blake3.Sum256([]byte(v))
		tag := hexEncode(sum[:])[:6]
		v = v[:32-1-len(tag)] + "_" + tag
	}
	return Slug{Value: v, Source: SourceMain}
}

// sanitiseBranch lowercases the branch name and replaces every
// non-alphanumeric run with a single underscore so the result is a
// valid identifier across MySQL/Postgres/Redis prefixes. Leading and
// trailing underscores are trimmed.
func sanitiseBranch(branch string) string {
	var b strings.Builder
	prevUnderscore := true
	for _, r := range strings.ToLower(branch) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// extractTicket pulls a Jira-style `[A-Z]+-\d+` ticket out of s and
// renders it `prefix_num` (lowercased). Length budgeting and the
// path-hash suffix are composeTicket's job, so no truncation or hashing
// happens here.
func extractTicket(s string) (string, bool) {
	caps := ticketRe.FindStringSubmatch(s)
	if caps == nil {
		return "", false
	}
	return strings.ToLower(caps[1]) + "_" + caps[2], true
}

// sysvCksum reproduces the POSIX `cksum` (SysV) algorithm. CRC-32
// over (data || length octets little-endian), polynomial
// 0x04C11DB7, initial 0, final NOT.
//
// We need this in-process because the bash hooks use POSIX `cksum`
// and treeman's redis indices must agree with hand-computed ones.
func sysvCksum(input []byte) uint32 {
	var crc uint32
	for _, b := range input {
		crc = crcUpdate(crc, b)
	}
	length := uint64(len(input))
	for length != 0 {
		crc = crcUpdate(crc, byte(length&0xff))
		length >>= 8
	}
	return ^crc
}

func crcUpdate(crc uint32, b byte) uint32 {
	crc ^= uint32(b) << 24
	for range 8 {
		if crc&0x80000000 != 0 {
			crc = (crc << 1) ^ 0x04C11DB7
		} else {
			crc <<= 1
		}
	}
	return crc
}

const hexDigits = "0123456789abcdef"

func hexEncode(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, hexDigits[x>>4], hexDigits[x&0x0f])
	}
	return string(out)
}
