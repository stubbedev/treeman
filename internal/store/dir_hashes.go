package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
)

// DirHashKey identifies one cached migration-directory hash.
type DirHashKey struct {
	Dir       string
	SpecName  string
	HashMode  string
	MtimeNs   int64
	MemberCnt int64
}

// DirHashRecord is what BatchDirHashes returns: the cached hash plus
// the (mtime_ns, member_count) tuple it was computed against. The
// caller compares both against the live stat to decide whether the
// row is still valid.
type DirHashRecord struct {
	MtimeNs     int64
	MemberCount int64
	MemberHash  string
}

// BatchDirHashes fetches cached dir_hashes rows for every (dir,
// spec_name, hash_mode) triple, in one round-trip. Returns a map
// keyed on the absolute dir path. Misses are silently absent.
//
// Caller compares the returned (mtime_ns, member_count) against the
// live stat / readdir result. A miss or a stale row → recompute and
// upsert via UpsertDirHash.
func (s *Store) BatchDirHashes(ctx context.Context, dirs []string, specName, hashMode string) (map[string]DirHashRecord, error) {
	out := make(map[string]DirHashRecord, len(dirs))
	if len(dirs) == 0 || specName == "" || hashMode == "" {
		return out, nil
	}
	const chunk = 500
	for i := 0; i < len(dirs); i += chunk {
		j := min(i+chunk, len(dirs))
		// A failed chunk is advisory: stop and return the partial cache
		// so the caller recomputes the rest.
		if !s.loadDirHashChunk(ctx, dirs[i:j], specName, hashMode, out) {
			return out, nil
		}
	}
	return out, nil
}

// loadDirHashChunk fills out with the cached rows for one chunk of
// dirs. Returns false on a query or iteration error (caller stops and
// recomputes the remaining dirs); an empty result set is a success.
func (s *Store) loadDirHashChunk(ctx context.Context, batch []string, specName, hashMode string, out map[string]DirHashRecord) bool {
	ph := make([]string, len(batch))
	args := make([]any, 0, len(batch)+2)
	for k, d := range batch {
		ph[k] = "?"
		args = append(args, d)
	}
	args = append(args, specName, hashMode)
	//nolint:gosec // only `?` placeholders are concatenated; values are parameterized
	q := "SELECT dir, mtime_ns, member_count, member_hash FROM dir_hashes " +
		"WHERE dir IN (" + strings.Join(ph, ",") + ") AND spec_name = ? AND hash_mode = ?"
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		slog.Warn("dir_hashes batch lookup failed", "err", err)
		return false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var dir, h string
		var mt, mc int64
		if err := rows.Scan(&dir, &mt, &mc, &h); err != nil {
			continue
		}
		out[dir] = DirHashRecord{MtimeNs: mt, MemberCount: mc, MemberHash: h}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("dir_hashes batch iteration failed", "err", err)
		return false
	}
	return true
}

// UpsertDirHashes writes (or refreshes) one row per entry.
func (s *Store) UpsertDirHashes(ctx context.Context, entries []DirHashKey, hashes map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO dir_hashes(dir, spec_name, hash_mode, mtime_ns, member_count, member_hash, cached_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(dir, spec_name, hash_mode) DO UPDATE SET
		    mtime_ns     = excluded.mtime_ns,
		    member_count = excluded.member_count,
		    member_hash  = excluded.member_hash,
		    cached_at    = excluded.cached_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	now := nowMillis()
	for _, e := range entries {
		h, ok := hashes[e.Dir]
		if !ok {
			continue
		}
		if _, err := stmt.ExecContext(ctx, e.Dir, e.SpecName, e.HashMode,
			e.MtimeNs, e.MemberCnt, h, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
