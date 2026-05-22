package daemon

import (
	"context"
	"log/slog"
	"time"
)

// walCheckpointInterval is the cadence at which WALCheckpointLoop
// forces a TRUNCATE checkpoint on the SQLite WAL. Five minutes
// matches the rate at which a busy treeman host typically chews
// through a few hundred event rows — long enough to absorb bursts
// inside one WAL page batch, short enough that `treeman.db-wal`
// never grows past a few MB.
const walCheckpointInterval = 5 * time.Minute

// WALCheckpointLoop runs `PRAGMA wal_checkpoint(TRUNCATE)` on a
// fixed cadence until ctx cancels. Required because modernc.org/
// sqlite's automatic passive checkpoints only fire at ~1000 WAL
// pages with synchronous=NORMAL, which on a daemon that writes
// constantly but rarely reaches that threshold can leave a stale
// WAL file growing indefinitely.
//
// TRUNCATE is the most aggressive mode: it checkpoints, waits for
// any readers to drain, then shrinks `-wal` back to zero bytes. If
// a reader is mid-transaction the checkpoint is partial and the
// next tick retries.
func WALCheckpointLoop(ctx context.Context, st *State) {
	t := time.NewTicker(walCheckpointInterval)
	defer t.Stop()
	slog.Info("wal_checkpoint_loop started", "interval", walCheckpointInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Store.CheckpointWAL(ctx); err != nil {
				slog.Warn("wal_checkpoint failed", "err", err)
			}
		}
	}
}
