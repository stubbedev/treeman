package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"lukechampine.com/blake3"
)

// HashedFile returns the BLAKE3-256 hex digest of path's contents.
// Uses the `file_hashes` cache table: when the on-disk (size,
// mtime_ns) matches the cached row, the disk read is skipped and
// the cached digest is returned directly. A miss recomputes and
// upserts the row.
//
// path must be absolute — caller's responsibility, so we don't have
// to disambiguate cache keys across cwd changes.
//
// This is the workhorse behind snapshot.LockfileHashesFor and the
// HashChecksum branch of framework.MigrationsHash, which previously
// rehashed every dump file + every migration file on every prepare.
func (s *Store) HashedFile(ctx context.Context, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("hashed file: not an absolute path: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("hashed file: %s is a directory", path)
	}
	size := info.Size()
	mtime := info.ModTime().UnixNano()

	var cachedSize, cachedMtime int64
	var cachedHash string
	row := s.DB.QueryRowContext(ctx,
		`SELECT size, mtime_ns, hash FROM file_hashes WHERE path = ?`, path)
	if err := row.Scan(&cachedSize, &cachedMtime, &cachedHash); err == nil {
		if cachedSize == size && cachedMtime == mtime {
			return cachedHash, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		// Cache lookup failed; fall through to compute. The cache is
		// advisory, so a transient SQLite error must not block the
		// hash itself.
	}

	hash, err := hashFileBLAKE3(path)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO file_hashes(path, size, mtime_ns, hash, cached_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET
		     size      = excluded.size,
		     mtime_ns  = excluded.mtime_ns,
		     hash      = excluded.hash,
		     cached_at = excluded.cached_at`,
		path, size, mtime, hash, nowMillis()); err != nil {
		// Cache write is advisory — the hash itself is still valid.
		// Log so disk-full / lock contention is visible instead of
		// silently degrading every subsequent prepare.
		slog.Warn("file_hashes cache update failed", "path", path, "err", err)
	}
	return hash, nil
}

// hashFileBLAKE3 streams path through a 256-bit BLAKE3 hasher.
// Streaming so multi-GB dumps don't double-RAM.
func hashFileBLAKE3(path string) (string, error) {
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
