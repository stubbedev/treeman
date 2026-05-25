// Package snapshot implements the snapshot cache + GC + paratest
// fan-out logic.
//
// A SnapshotKey is the cache key the prepare orchestrator uses to
// look up a built template DB: identical (engine, source DB name,
// migrations_hash, dump_hash, lockfile_hashes) means an existing
// template can be restored instead of rebuilt from scratch.
package snapshot

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"lukechampine.com/blake3"
)

// FormatVersion bumps any time the SnapshotKey layout changes in a
// way that invalidates existing fingerprints. v2 swapped sha256 →
// blake3 across both the fingerprint hash and the per-file content
// hashes that feed it. v3 hashes the canonical field stream directly
// into blake3 (no JSON intermediate). v4 dropped the Framework field
// (framework dispatch was removed; the migrate command is declared
// per-repo in YAML). Every existing _tm_… template rebuilds once
// after each bump.
const FormatVersion uint32 = 4

// Key fingerprints a snapshot. Two prepare runs that share every
// field can reuse the same template DB.
type Key struct {
	FormatVersion     uint32            `json:"format_version"`
	Engine            string            `json:"engine"`
	EngineVersion     string            `json:"engine_version"`
	SourceDBName      string            `json:"source_db_name"`
	HashMode          string            `json:"hash_mode"`
	MigrationsHashHex string            `json:"migrations_hash_hex"`
	DumpHashHex       string            `json:"dump_hash_hex,omitempty"`
	LockfileHashes    map[string]string `json:"lockfile_hashes"`
}

// New constructs a Key with the format version pinned.
func New(engine, engineVersion, sourceDB, hashMode, migrationsHash, dumpHash string, lockfileHashes map[string]string) Key {
	if lockfileHashes == nil {
		lockfileHashes = map[string]string{}
	}
	return Key{
		FormatVersion:     FormatVersion,
		Engine:            engine,
		EngineVersion:     engineVersion,
		SourceDBName:      sourceDB,
		HashMode:          hashMode,
		MigrationsHashHex: migrationsHash,
		DumpHashHex:       dumpHash,
		LockfileHashes:    lockfileHashes,
	}
}

// Fingerprint returns a stable hex-encoded blake3 digest of the
// key. The hash input is a canonical, NUL-delimited stream of the
// fields in declaration order — no JSON marshal, no intermediate
// allocations beyond the lockfile-name sort.
//
// Per-field encoding:
//
//	uint32 FormatVersion  : 4 bytes little-endian, then '\x00'
//	strings               : raw bytes, terminated by '\x00'
//	lockfile_hashes       : uint32 count LE, then for each entry in
//	                        sorted-key order:  name '\x00' value '\x00'
//
// The choice of separator is irrelevant for collision resistance
// (blake3 isn't length-extension vulnerable), but the NUL keeps the
// input unambiguously parseable for debug/audit dumps.
func (k Key) Fingerprint() string {
	h := blake3.New(32, nil)
	var vbuf [4]byte
	binary.LittleEndian.PutUint32(vbuf[:], k.FormatVersion)
	_, _ = h.Write(vbuf[:])
	_, _ = h.Write([]byte{0})
	for _, s := range []string{
		k.Engine,
		k.EngineVersion,
		k.SourceDBName,
		k.HashMode,
		k.MigrationsHashHex,
		k.DumpHashHex,
	} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	keys := make([]string, 0, len(k.LockfileHashes))
	for name := range k.LockfileHashes {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	var cbuf [4]byte
	binary.LittleEndian.PutUint32(cbuf[:], uint32(len(keys)))
	_, _ = h.Write(cbuf[:])
	_, _ = h.Write([]byte{0})
	for _, name := range keys {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(k.LockfileHashes[name]))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TemplateName returns the synthesized template-DB name used by the
// prepare path: `_tm_<fingerprint[0:16]>`. Short, hyphen-free + stable
// so identifier-validation rules never reject it.
//
// Only used at template *creation* time — the SQLite snapshots table
// stores the resulting name in `template_name` and every read path
// (cache hit lookup, GC eviction, restore) reads from there. The
// engine kind isn't encoded in the name because `snapshots.engine`
// already carries it, and the `_tmpl_` token was redundant with the
// `_tm_` namespace marker. Old `_tm_tmpl_<engine>_*` names from
// earlier builds stay valid: existing rows continue to round-trip
// through `template_name`, and only newly built templates get the
// shorter form.
func (k Key) TemplateName() string {
	return fmt.Sprintf("_tm_%s", k.Fingerprint()[:16])
}

// IndexPrefix is the ES/OpenSearch variant of TemplateName: ES
// forbids index names starting with `_`, so we drop the leading
// namespace marker. Returns a stable, lowercase, hyphen-free prefix
// without trailing punctuation; callers add their own separator.
func (k Key) IndexPrefix() string {
	return fmt.Sprintf("tm_%s", k.Fingerprint()[:16])
}

// HashCache is the subset of *store.Store that LockfileHashesFor +
// MigrationsHash need: a stat-gated cache that returns hex digests
// for absolute paths. Defined here so internal/snapshot doesn't
// import internal/store (avoiding a cycle when other store consumers
// want to import snapshot).
type HashCache interface {
	HashedFile(ctx context.Context, path string) (string, error)
	BatchHashedFiles(ctx context.Context, paths []string) (map[string]string, error)
}

// LockfileHashesFor hashes a list of files. Missing files are
// skipped (not an error) so configs that name optional lockfiles
// don't fail when a particular project doesn't ship them.
//
// When cache is non-nil, hashes are looked up via the stat-gated
// SQLite cache so unchanged files (matching size + mtime_ns) skip
// the disk read. Pass nil to force an uncached read (test paths,
// out-of-daemon CLI flows).
func LockfileHashesFor(paths []string) (map[string]string, error) {
	return LockfileHashesForWithCache(context.Background(), nil, paths)
}

// LockfileHashesForWithCache is the cache-aware variant. See
// LockfileHashesFor for semantics. With a non-nil cache, all paths
// fold into a single SELECT IN(?,?,…) round-trip; misses hash in
// parallel and back-fill the cache before returning.
func LockfileHashesForWithCache(ctx context.Context, cache HashCache, paths []string) (map[string]string, error) {
	out := map[string]string{}
	if cache != nil {
		absPaths := make([]string, 0, len(paths))
		baseOf := make(map[string]string, len(paths))
		for _, p := range paths {
			info, err := os.Stat(p)
			if err != nil || info.IsDir() {
				continue
			}
			abs, err := filepath.Abs(p)
			if err != nil {
				continue
			}
			absPaths = append(absPaths, abs)
			baseOf[abs] = filepath.Base(p)
		}
		hashes, err := cache.BatchHashedFiles(ctx, absPaths)
		if err == nil {
			for abs, h := range hashes {
				if name, ok := baseOf[abs]; ok {
					out[name] = h
				}
			}
			return out, nil
		}
		// Cache failure — fall through to uncached path so the prepare
		// keeps moving even on transient SQLite errors.
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		h, err := hashFile(p)
		if err != nil {
			return out, err
		}
		out[filepath.Base(p)] = h
	}
	return out, nil
}

// hashFile streams path through a 256-bit BLAKE3 hasher.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
