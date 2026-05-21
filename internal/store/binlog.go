package store

import (
	"context"
	"database/sql"
	"errors"
)

// BinlogCheckpoint mirrors a row in `binlog_checkpoints`. Either
// (BinlogFile, BinlogPos) or GtidSet is populated depending on the
// upstream server's replication mode.
type BinlogCheckpoint struct {
	RepoID      int64
	SourceDB    string
	Flavor      string
	BinlogFile  string
	BinlogPos   uint32
	GtidSet     string
	LastEventTs int64
	UpdatedAt   int64
}

// GetBinlogCheckpoint returns the persisted cursor for (repoID,
// sourceDB), or (nil, nil) if none exists yet.
func (s *Store) GetBinlogCheckpoint(ctx context.Context, repoID int64, sourceDB string) (*BinlogCheckpoint, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT repo_id, source_db, flavor,
		       COALESCE(binlog_file,''), COALESCE(binlog_pos,0),
		       COALESCE(gtid_set,''), COALESCE(last_event_ts,0), updated_at
		FROM binlog_checkpoints WHERE repo_id = ? AND source_db = ?`,
		repoID, sourceDB)
	var cp BinlogCheckpoint
	var pos int64
	err := row.Scan(&cp.RepoID, &cp.SourceDB, &cp.Flavor,
		&cp.BinlogFile, &pos, &cp.GtidSet, &cp.LastEventTs, &cp.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cp.BinlogPos = uint32(pos)
	return &cp, nil
}

// SaveBinlogCheckpoint upserts the cursor. Called by the replicator
// after every successfully-applied event so daemon restart can
// resume from the last-applied position.
func (s *Store) SaveBinlogCheckpoint(ctx context.Context, cp BinlogCheckpoint) error {
	now := nowMillis()
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO binlog_checkpoints
		    (repo_id, source_db, flavor, binlog_file, binlog_pos, gtid_set, last_event_ts, updated_at)
		VALUES (?, ?, ?, NULLIF(?,''), NULLIF(?,0), NULLIF(?,''), NULLIF(?,0), ?)
		ON CONFLICT(repo_id, source_db) DO UPDATE SET
		    flavor        = excluded.flavor,
		    binlog_file   = excluded.binlog_file,
		    binlog_pos    = excluded.binlog_pos,
		    gtid_set      = excluded.gtid_set,
		    last_event_ts = excluded.last_event_ts,
		    updated_at    = excluded.updated_at`,
		cp.RepoID, cp.SourceDB, cp.Flavor,
		cp.BinlogFile, cp.BinlogPos, cp.GtidSet, cp.LastEventTs, now)
	return err
}
