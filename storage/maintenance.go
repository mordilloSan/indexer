package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mordilloSan/go_logger/logger"
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

// PruneStats holds statistics about the pruning operation
type PruneStats struct {
	DeletedIndexes int
	DeletedEntries int64
	Duration       time.Duration
}

// PruneOldIndexes removes index records that haven't been updated in the specified retention period.
// This also cascades to delete all associated entries due to the FOREIGN KEY constraint.
// keepLatest specifies how many most recent indexes to always keep (minimum 1).
// maxAge specifies the maximum age for indexes to keep (e.g., 30 days).
func PruneOldIndexes(ctx context.Context, db *sql.DB, keepLatest int, maxAge time.Duration) (PruneStats, error) {
	ctx = ensureContext(ctx)
	if db == nil {
		return PruneStats{}, fmt.Errorf("db is nil")
	}
	if keepLatest < 1 {
		keepLatest = 1
	}

	start := time.Now()
	var stats PruneStats

	// Calculate cutoff timestamp
	cutoffTime := time.Now().Add(-maxAge).Unix()

	// First, count entries that will be deleted (for stats)
	var entriesToDelete int64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(num_files + num_dirs), 0)
		FROM indexes
		WHERE id NOT IN (
			SELECT id FROM indexes ORDER BY last_indexed DESC LIMIT ?
		)
		AND last_indexed < ?;
	`, keepLatest, cutoffTime).Scan(&entriesToDelete)
	if err != nil {
		return PruneStats{}, fmt.Errorf("count entries to delete: %w", err)
	}

	// Delete old indexes (entries will cascade delete)
	result, err := db.ExecContext(ctx, `
		DELETE FROM indexes
		WHERE id NOT IN (
			SELECT id FROM indexes ORDER BY last_indexed DESC LIMIT ?
		)
		AND last_indexed < ?;
	`, keepLatest, cutoffTime)
	if err != nil {
		return PruneStats{}, fmt.Errorf("delete old indexes: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return PruneStats{}, fmt.Errorf("get rows affected: %w", err)
	}

	stats.DeletedIndexes = int(deleted)
	stats.DeletedEntries = entriesToDelete
	stats.Duration = time.Since(start).Truncate(time.Millisecond)

	// Run incremental vacuum to reclaim space
	if _, err := db.ExecContext(ctx, `PRAGMA incremental_vacuum;`); err != nil {
		// Log warning but don't fail the operation
		logger.Warnf("Incremental vacuum failed after pruning: %v", err)
	}

	return stats, nil
}
