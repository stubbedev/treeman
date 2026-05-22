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

// For consults `branch` first (a ticket-named branch wins even when
// the worktree directory was named generically). Then the worktree's
// own basename. Falls back to hashing the canonical path so two
// worktrees that happen to share a last path component still get
// distinct slugs.
func For(worktreePath string, branch string) Slug {
	if branch != "" {
		if s, ok := extractTicket(branch); ok {
			return Slug{Value: s, Source: SourceTicket}
		}
	}
	base := filepath.Base(worktreePath)
	if s, ok := extractTicket(base); ok {
		return Slug{Value: s, Source: SourceTicket}
	}
	canonical, err := filepath.Abs(worktreePath)
	if err != nil {
		canonical = worktreePath
	}
	sum := blake3.Sum256([]byte(canonical))
	hex := hexEncode(sum[:])
	return Slug{Value: "wt_" + hex[:8], Source: SourcePathHash}
}

func extractTicket(s string) (string, bool) {
	caps := ticketRe.FindStringSubmatch(s)
	if caps == nil {
		return "", false
	}
	prefix := strings.ToLower(caps[1])
	num := caps[2]
	out := prefix + "_" + num
	if len(out) > 32 {
		// Naive truncation can collide two distinct tickets
		// (proj_looong_12345 vs proj_looong_12346). Append a short
		// hash of the full ticket so the 32-char budget still
		// distinguishes them.
		sum := blake3.Sum256([]byte(out))
		tag := hexEncode(sum[:])[:6]
		out = out[:32-1-len(tag)] + "_" + tag
	}
	return out, true
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
	for i := 0; i < 8; i++ {
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
