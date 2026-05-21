// Package snapshot implements the snapshot cache + GC + paratest
// fan-out logic.
//
// A SnapshotKey is the cache key the prepare orchestrator uses to
// look up a built template DB: identical (engine, framework,
// migrations_hash, dump_hash, lockfile_hashes) means an existing
// template can be restored instead of rebuilt from scratch.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// FormatVersion bumps any time the SnapshotKey layout changes in a
// way that invalidates existing fingerprints.
const FormatVersion uint32 = 1

// Key fingerprints a snapshot. Two prepare runs that share every
// field can reuse the same template DB.
type Key struct {
	FormatVersion     uint32            `json:"format_version"`
	Engine            string            `json:"engine"`
	EngineVersion     string            `json:"engine_version"`
	SourceDBName      string            `json:"source_db_name"`
	Framework         string            `json:"framework"`
	HashMode          string            `json:"hash_mode"`
	MigrationsHashHex string            `json:"migrations_hash_hex"`
	DumpHashHex       string            `json:"dump_hash_hex,omitempty"`
	LockfileHashes    map[string]string `json:"lockfile_hashes"`
}

// New constructs a Key with the format version pinned.
func New(engine, engineVersion, sourceDB, framework, hashMode, migrationsHash, dumpHash string, lockfileHashes map[string]string) Key {
	if lockfileHashes == nil {
		lockfileHashes = map[string]string{}
	}
	return Key{
		FormatVersion:     FormatVersion,
		Engine:            engine,
		EngineVersion:     engineVersion,
		SourceDBName:      sourceDB,
		Framework:         framework,
		HashMode:          hashMode,
		MigrationsHashHex: migrationsHash,
		DumpHashHex:       dumpHash,
		LockfileHashes:    lockfileHashes,
	}
}

// Fingerprint returns a stable hex-encoded hash of the key. sha256
// is fine here — the fingerprint is consumed only by treeman itself
// (the SQLite store keys on whatever the binary writes), so the
// choice of hash never crosses a compatibility boundary.
func (k Key) Fingerprint() string {
	// Sort lockfile keys so JSON encoding is canonical.
	type stableKey struct {
		FormatVersion     uint32      `json:"format_version"`
		Engine            string      `json:"engine"`
		EngineVersion     string      `json:"engine_version"`
		SourceDBName      string      `json:"source_db_name"`
		Framework         string      `json:"framework"`
		HashMode          string      `json:"hash_mode"`
		MigrationsHashHex string      `json:"migrations_hash_hex"`
		DumpHashHex       string      `json:"dump_hash_hex,omitempty"`
		LockfileHashes    [][2]string `json:"lockfile_hashes"`
	}
	sk := stableKey{
		FormatVersion:     k.FormatVersion,
		Engine:            k.Engine,
		EngineVersion:     k.EngineVersion,
		SourceDBName:      k.SourceDBName,
		Framework:         k.Framework,
		HashMode:          k.HashMode,
		MigrationsHashHex: k.MigrationsHashHex,
		DumpHashHex:       k.DumpHashHex,
	}
	keys := make([]string, 0, len(k.LockfileHashes))
	for name := range k.LockfileHashes {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		sk.LockfileHashes = append(sk.LockfileHashes, [2]string{name, k.LockfileHashes[name]})
	}
	b, err := json.Marshal(sk)
	if err != nil {
		// Inputs are trivially encodable; panic surfaces a
		// programming error.
		panic(fmt.Sprintf("snapshot key: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// LockfileHashesFor hashes a list of files. Missing files are
// skipped (not an error) so configs that name optional lockfiles
// don't fail when a particular project doesn't ship them.
func LockfileHashesFor(paths []string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return out, err
		}
		sum := sha256.Sum256(b)
		out[filepath.Base(p)] = hex.EncodeToString(sum[:])
	}
	return out, nil
}
