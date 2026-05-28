package store

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
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
//
// Callers hashing many paths in one go should prefer
// BatchHashedFiles, which folds the lookups into a single SELECT IN
// and parallelizes only the misses.
func (s *Store) HashedFile(ctx context.Context, path string) (string, error) {
	hashes, err := s.BatchHashedFiles(ctx, []string{path})
	if err != nil {
		return "", err
	}
	h, ok := hashes[path]
	if !ok {
		return "", fmt.Errorf("hashed file: no result for %s", path)
	}
	return h, nil
}

// BatchHashedFiles returns BLAKE3-256 hex digests for every path
// in `paths`. Paths must be absolute.
//
// Strategy:
//  1. stat each path once (skip non-files / missing).
//  2. one SELECT … WHERE path IN(?,?,…) to fetch cached rows.
//  3. for cached rows whose (size, mtime_ns) matches the live stat,
//     return immediately. Refresh `cached_at` so LRU keeps hot files.
//  4. for misses, consult the content-addressable secondary index
//     (size, mtime_ns) — same content across worktrees reuses an
//     existing row without re-reading bytes.
//  5. for true misses, compute BLAKE3 in parallel (GOMAXPROCS) and
//     upsert each fresh row.
//
// Returns a map keyed on the input absolute path. Missing or
// non-regular files are silently absent from the result (matches the
// per-path callers' behaviour).
func (s *Store) BatchHashedFiles(ctx context.Context, paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	if len(paths) == 0 {
		return out, nil
	}

	stats := make(map[string]struct {
		size  int64
		mtime int64
	}, len(paths))
	live := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("hashed file: not an absolute path: %s", p)
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		stats[p] = struct {
			size  int64
			mtime int64
		}{size: info.Size(), mtime: info.ModTime().UnixNano()}
		live = append(live, p)
	}
	if len(live) == 0 {
		return out, nil
	}

	cached := make(map[string]struct {
		size  int64
		mtime int64
		hash  string
	}, len(live))

	// SQLite ships with a default SQLITE_MAX_VARIABLE_NUMBER of 32766
	// (modernc/sqlite), but chunk anyway so really large prepare
	// sets stay safe.
	const chunk = 500
	for i := 0; i < len(live); i += chunk {
		j := i + chunk
		if j > len(live) {
			j = len(live)
		}
		batch := live[i:j]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for k, p := range batch {
			ph[k] = "?"
			args[k] = p
		}
		q := "SELECT path, size, mtime_ns, hash FROM file_hashes WHERE path IN (" + strings.Join(ph, ",") + ")"
		rows, err := s.DB.QueryContext(ctx, q, args...)
		if err != nil {
			// Cache lookup failure is advisory — fall through to
			// recompute everything below.
			slog.Warn("file_hashes batch lookup failed", "err", err)
			break
		}
		for rows.Next() {
			var p, h string
			var sz, mt int64
			if err := rows.Scan(&p, &sz, &mt, &h); err != nil {
				continue
			}
			cached[p] = struct {
				size  int64
				mtime int64
				hash  string
			}{sz, mt, h}
		}
		_ = rows.Close()
	}

	now := nowMillis()
	var (
		hits     []string
		misses   []string
		secondHi []struct {
			path, hash string
			size, mt   int64
		}
	)

	for _, p := range live {
		st := stats[p]
		if c, ok := cached[p]; ok && c.size == st.size && c.mtime == st.mtime {
			out[p] = c.hash
			hits = append(hits, p)
			continue
		}
		misses = append(misses, p)
	}

	// Secondary (content-addressable) lookup for misses: any row with
	// the same (size, mtime_ns) lets us reuse its hash without a disk
	// read. Resolved in one batched query per chunk of distinct pairs
	// rather than a QueryRow per miss — on a cold prepare every file is
	// a miss, so the per-miss form fired N round trips (each returning
	// nothing while the table is still empty).
	if len(misses) > 0 {
		type pair struct{ size, mtime int64 }
		// byPair maps a distinct (size,mtime) to its reusable hash
		// ("" once seen, filled when the query finds a match).
		byPair := make(map[pair]string, len(misses))
		distinct := make([]pair, 0, len(misses))
		for _, p := range misses {
			st := stats[p]
			k := pair{st.size, st.mtime}
			if _, ok := byPair[k]; !ok {
				byPair[k] = ""
				distinct = append(distinct, k)
			}
		}
		// 2 placeholders per pair; cap each chunk well under SQLite's
		// 32766 variable limit.
		const pairChunk = 400
		for i := 0; i < len(distinct); i += pairChunk {
			j := i + pairChunk
			if j > len(distinct) {
				j = len(distinct)
			}
			batch := distinct[i:j]
			clauses := make([]string, len(batch))
			args := make([]any, 0, len(batch)*2)
			for k, pr := range batch {
				clauses[k] = "(size = ? AND mtime_ns = ?)"
				args = append(args, pr.size, pr.mtime)
			}
			q := "SELECT size, mtime_ns, hash FROM file_hashes WHERE " + strings.Join(clauses, " OR ")
			rows, err := s.DB.QueryContext(ctx, q, args...)
			if err != nil {
				// Advisory — fall through and hash these as true misses.
				slog.Warn("file_hashes secondary batch lookup failed", "err", err)
				break
			}
			for rows.Next() {
				var sz, mt int64
				var h string
				if err := rows.Scan(&sz, &mt, &h); err != nil {
					continue
				}
				if h == "" {
					continue
				}
				k := pair{sz, mt}
				if cur, ok := byPair[k]; ok && cur == "" {
					byPair[k] = h
				}
			}
			_ = rows.Close()
		}
		// Map each miss back to its pair's hash, preserving order. In-
		// place filter (stillMissing aliases misses' backing array) is
		// safe: append never outruns the read cursor.
		stillMissing := misses[:0]
		for _, p := range misses {
			st := stats[p]
			if h := byPair[pair{st.size, st.mtime}]; h != "" {
				out[p] = h
				secondHi = append(secondHi, struct {
					path, hash string
					size, mt   int64
				}{p, h, st.size, st.mtime})
				continue
			}
			stillMissing = append(stillMissing, p)
		}
		misses = stillMissing
	}

	// Hash true misses in parallel.
	if len(misses) > 0 {
		limit := runtime.GOMAXPROCS(0)
		if limit < 1 {
			limit = 1
		}
		var mu sync.Mutex
		fresh := make(map[string]string, len(misses))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(limit)
		for _, p := range misses {
			p := p
			g.Go(func() error {
				h, err := hashFileBLAKE3(p)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return nil
					}
					return err
				}
				_ = gctx
				mu.Lock()
				fresh[p] = h
				mu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return out, err
		}
		for p, h := range fresh {
			out[p] = h
		}
		// Upsert fresh rows + secondary-index hits as their own
		// rows so future runs hit by path directly.
		if err := s.upsertHashRows(ctx, fresh, stats, now); err != nil {
			slog.Warn("file_hashes batch upsert failed", "err", err)
		}
	}
	if len(secondHi) > 0 {
		rowMap := make(map[string]string, len(secondHi))
		statMap := make(map[string]struct {
			size  int64
			mtime int64
		}, len(secondHi))
		for _, e := range secondHi {
			rowMap[e.path] = e.hash
			statMap[e.path] = struct {
				size  int64
				mtime int64
			}{size: e.size, mtime: e.mt}
		}
		if err := s.upsertHashRows(ctx, rowMap, statMap, now); err != nil {
			slog.Warn("file_hashes secondary upsert failed", "err", err)
		}
	}
	if len(hits) > 0 {
		// Refresh cached_at so age-based pruning keeps hot files.
		if err := s.touchHashRows(ctx, hits, now); err != nil {
			slog.Warn("file_hashes touch failed", "err", err)
		}
	}
	return out, nil
}

func (s *Store) upsertHashRows(ctx context.Context, hashes map[string]string,
	stats map[string]struct {
		size  int64
		mtime int64
	}, now int64) error {
	if len(hashes) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO file_hashes(path, size, mtime_ns, hash, cached_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		    size      = excluded.size,
		    mtime_ns  = excluded.mtime_ns,
		    hash      = excluded.hash,
		    cached_at = excluded.cached_at`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for p, h := range hashes {
		st := stats[p]
		if _, err := stmt.ExecContext(ctx, p, st.size, st.mtime, h, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) touchHashRows(ctx context.Context, paths []string, now int64) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `UPDATE file_hashes SET cached_at = ? WHERE path = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, p := range paths {
		if _, err := stmt.ExecContext(ctx, now, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// hashFileBLAKE3 streams path through a 256-bit BLAKE3 hasher.
// Streaming so multi-GB dumps don't double-RAM.
func hashFileBLAKE3(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
