package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// ConfigGeneration is one historical snapshot of a config file taken
// just before it was overwritten. Content holds the bytes as they were
// before the write; Generation is the per-(repo,path) monotonic handle
// surfaced by `treeman config history|restore`.
type ConfigGeneration struct {
	ID         int64
	RepoID     int64
	Path       string
	Generation int64
	Content    []byte
	CreatedAt  int64
}

// SnapshotConfig records the current on-disk content of configPath as a
// new generation for the repo rooted at repoRoot, before the caller
// overwrites it. content is the existing file bytes (read by the
// caller). Returns the assigned generation number.
//
// Replaces the old `<path>.bak.<timestamp>` file-in-project-root scheme:
// callers now stash history in SQLite and write the new content with a
// plain atomic write.
func (s *Store) SnapshotConfig(ctx context.Context, repoRoot, configPath string, content []byte) (int64, error) {
	repoID, err := s.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return 0, fmt.Errorf("ensure repo for config snapshot: %w", err)
	}
	var next int64
	row := s.DB.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(generation), 0) + 1 FROM config_generations WHERE repo_id = ? AND path = ?",
		repoID, configPath)
	if err := row.Scan(&next); err != nil {
		return 0, err
	}
	if _, err := s.DB.ExecContext(ctx,
		"INSERT INTO config_generations(repo_id, path, generation, content, created_at) VALUES (?, ?, ?, ?, ?)",
		repoID, configPath, next, content, nowMillis()); err != nil {
		return 0, err
	}
	return next, nil
}

// ListConfigGenerations returns every stored generation for configPath
// under repoRoot, newest first. Empty (not an error) when none exist.
func (s *Store) ListConfigGenerations(ctx context.Context, repoRoot, configPath string) ([]ConfigGeneration, error) {
	repoID, err := s.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(
		ctx,
		"SELECT id, repo_id, path, generation, content, created_at FROM config_generations WHERE repo_id = ? AND path = ? ORDER BY generation DESC",
		repoID,
		configPath,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ConfigGeneration
	for rows.Next() {
		var g ConfigGeneration
		if err := rows.Scan(&g.ID, &g.RepoID, &g.Path, &g.Generation, &g.Content, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetConfigGeneration fetches one generation by its per-(repo,path)
// number. Returns sql.ErrNoRows when the generation doesn't exist.
func (s *Store) GetConfigGeneration(ctx context.Context, repoRoot, configPath string, generation int64) (ConfigGeneration, error) {
	repoID, err := s.EnsureRepo(ctx, repoRoot, filepath.Base(repoRoot))
	if err != nil {
		return ConfigGeneration{}, err
	}
	var g ConfigGeneration
	row := s.DB.QueryRowContext(
		ctx,
		"SELECT id, repo_id, path, generation, content, created_at FROM config_generations WHERE repo_id = ? AND path = ? AND generation = ?",
		repoID,
		configPath,
		generation,
	)
	if err := row.Scan(&g.ID, &g.RepoID, &g.Path, &g.Generation, &g.Content, &g.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfigGeneration{}, err
		}
		return ConfigGeneration{}, err
	}
	return g, nil
}
