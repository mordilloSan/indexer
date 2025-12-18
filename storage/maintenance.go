package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type WALCheckpointStats struct {
	Busy         int
	Log          int
	Checkpointed int
	Duration     time.Duration
}

// WALCheckpointTruncate checkpoints the WAL and truncates the -wal file.
// This helps prevent unbounded WAL growth in long-running processes.
func WALCheckpointTruncate(ctx context.Context, db *sql.DB) (WALCheckpointStats, error) {
	ctx = ensureContext(ctx)
	if db == nil {
		return WALCheckpointStats{}, fmt.Errorf("db is nil")
	}

	start := time.Now()
	var stats WALCheckpointStats
	err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE);`).Scan(&stats.Busy, &stats.Log, &stats.Checkpointed)
	stats.Duration = time.Since(start).Truncate(time.Millisecond)
	if err != nil {
		return WALCheckpointStats{}, err
	}
	return stats, nil
}

type VacuumStats struct {
	Duration time.Duration
}

// Vacuum rebuilds the SQLite database file to reclaim free space and defragment pages.
// Note: VACUUM requires an exclusive lock and can be slow on large databases.
func Vacuum(ctx context.Context, db *sql.DB) (VacuumStats, error) {
	ctx = ensureContext(ctx)
	if db == nil {
		return VacuumStats{}, fmt.Errorf("db is nil")
	}

	start := time.Now()
	if _, err := db.ExecContext(ctx, `VACUUM;`); err != nil {
		return VacuumStats{}, err
	}
	return VacuumStats{Duration: time.Since(start).Truncate(time.Millisecond)}, nil
}
