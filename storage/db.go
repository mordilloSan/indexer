package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/indexing"
)

const (
	defaultDBPath = "indexer.db"
	busyTimeoutMS = 5000
	schemaTimeout = 30 * time.Second
	batchSize     = 500
	batchTimeout  = 1 * time.Second
)

// dbExecutor is an interface that both sql.DB and sql.Tx implement
type dbExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ProgressCallback is called after each batch write with cumulative counts and last path processed
type ProgressCallback func(filesWritten, dirsWritten int64, lastPath string)

// StreamingWriter accepts entries via a channel and writes them to the database in batches.
type StreamingWriter struct {
	db           *sql.DB
	indexID      int64
	scanTime     int64
	entryCh      chan indexing.IndexEntry
	doneCh       chan error
	ctx          context.Context
	cancel       context.CancelFunc
	errVal       atomic.Value
	progressCb   ProgressCallback
	filesWritten int64
	dirsWritten  int64
	lastPath     string
}

// NewStreamingWriter creates a writer that batches entries and commits periodically.
// The provided ctx allows callers to cancel the writer even if Close is not reached.
func NewStreamingWriter(ctx context.Context, db *sql.DB, indexID int64, bufferSize int) *StreamingWriter {
	return NewStreamingWriterWithProgress(ctx, db, indexID, bufferSize, nil)
}

// NewStreamingWriterWithProgress creates a writer with an optional progress callback.
// The callback is invoked after each entry is processed with cumulative file/dir counts.
func NewStreamingWriterWithProgress(ctx context.Context, db *sql.DB, indexID int64, bufferSize int, progressCb ProgressCallback) *StreamingWriter {
	if ctx == nil {
		ctx = context.Background()
	}
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	ctx, cancel := context.WithCancel(ctx)
	sw := &StreamingWriter{
		db:         db,
		indexID:    indexID,
		scanTime:   time.Now().UTC().Unix(),
		entryCh:    make(chan indexing.IndexEntry, bufferSize),
		doneCh:     make(chan error, 1),
		ctx:        ctx,
		cancel:     cancel,
		progressCb: progressCb,
	}
	go sw.run()
	return sw
}

// Write sends an entry to be written to the database.
func (sw *StreamingWriter) Write(entry indexing.IndexEntry) error {
	select {
	case sw.entryCh <- entry:
		return nil
	case <-sw.ctx.Done():
		if v, ok := sw.errVal.Load().(error); ok && v != nil {
			return v
		}
		return sw.ctx.Err()
	}
}

// Close signals completion and waits for all pending writes to finish.
func (sw *StreamingWriter) Close() error {
	close(sw.entryCh)
	return <-sw.doneCh
}

// DB returns the underlying database connection for direct operations.
func (sw *StreamingWriter) DB() *sql.DB {
	return sw.db
}

// IndexID returns the index ID this writer is associated with.
func (sw *StreamingWriter) IndexID() int64 {
	return sw.indexID
}

// ScanTime returns the timestamp when this scan started.
func (sw *StreamingWriter) ScanTime() int64 {
	return sw.scanTime
}

// run is the background goroutine that batches and writes entries.
func (sw *StreamingWriter) run() {
	var err error
	defer func() {
		if err != nil {
			sw.errVal.Store(err)
		}
		sw.doneCh <- err
		sw.cancel()
	}()

	batch := make([]indexing.IndexEntry, 0, batchSize)
	ticker := time.NewTicker(batchTimeout)
	defer ticker.Stop()

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if e := sw.writeBatch(batch); e != nil {
			return e
		}
		// Clear backing array to release string references
		for i := range batch {
			batch[i] = indexing.IndexEntry{}
		}
		batch = batch[:0]
		return nil
	}

	for {
		select {
		case entry, ok := <-sw.entryCh:
			if !ok {
				// Channel closed, flush remaining and exit
				err = flush()
				return
			}
			// Track progress
			if entry.Type == "directory" {
				sw.dirsWritten++
			} else {
				sw.filesWritten++
			}
			sw.lastPath = entry.RelativePath

			// Call progress callback if set
			if sw.progressCb != nil {
				sw.progressCb(sw.filesWritten, sw.dirsWritten, sw.lastPath)
			}

			batch = append(batch, entry)
			if len(batch) >= batchSize {
				if err = flush(); err != nil {
					return
				}
			}
		case <-ticker.C:
			if err = flush(); err != nil {
				return
			}
		case <-sw.ctx.Done():
			err = sw.ctx.Err()
			return
		}
	}
}

// writeBatch writes a batch of entries within a single transaction.
func (sw *StreamingWriter) writeBatch(batch []indexing.IndexEntry) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := sw.db.BeginTx(sw.ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	err = insertEntriesBatch(sw.ctx, tx, sw.indexID, sw.scanTime, batch)
	if err != nil {
		return err
	}

	err = tx.Commit()
	return err
}

// Open creates (or reuses) a SQLite database and ensures the schema exists.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		path = defaultDBPath
	}
	// Use WAL for concurrent readers while streaming writes happen.
	// synchronous=OFF for maximum write performance during indexing
	// auto_vacuum=INCREMENTAL to automatically reclaim space when deleting records
	dsn := fmt.Sprintf("%s?_busy_timeout=%d&_foreign_keys=on&_journal_mode=WAL&_synchronous=OFF&_auto_vacuum=INCREMENTAL", path, busyTimeoutMS)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()

	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL;`).Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Configure connection pool for WAL mode concurrent access
	// WAL mode allows multiple readers + 1 writer simultaneously
	db.SetMaxOpenConns(5)                  // Allow up to 5 concurrent connections
	db.SetMaxIdleConns(2)                  // Keep 2 connections ready
	db.SetConnMaxLifetime(0)               // Reuse connections indefinitely while active
	db.SetConnMaxIdleTime(5 * time.Minute) // Close idle connections to release SQLite caches

	if err := initSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// GetJournalMode returns the SQLite journal mode for the provided database.
func GetJournalMode(ctx context.Context, db *sql.DB) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if db == nil {
		return "", fmt.Errorf("db is nil")
	}

	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode;`).Scan(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

func initSchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON;`); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS indexes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			root_path TEXT NOT NULL,
			source TEXT,
			include_hidden INTEGER NOT NULL DEFAULT 0,
			num_dirs INTEGER NOT NULL DEFAULT 0,
			num_files INTEGER NOT NULL DEFAULT 0,
			total_size INTEGER NOT NULL DEFAULT 0,
			disk_used INTEGER NOT NULL DEFAULT 0,
			disk_total INTEGER NOT NULL DEFAULT 0,
			last_indexed INTEGER NOT NULL,
			index_duration_ms INTEGER NOT NULL DEFAULT 0,
			export_duration_ms INTEGER NOT NULL DEFAULT 0,
			vacuum_duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		);
	`); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			index_id INTEGER NOT NULL,
			relative_path TEXT NOT NULL,
			path_depth INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			size INTEGER NOT NULL,
			mod_time INTEGER NOT NULL,
			type TEXT NOT NULL,
			hidden INTEGER NOT NULL DEFAULT 0,
			inode INTEGER NOT NULL DEFAULT 0,
			last_seen INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (index_id) REFERENCES indexes(id) ON DELETE CASCADE
		);
	`); err != nil {
		return err
	}

	// Breaking schema change: absolute_path is no longer stored.
	// If an existing database still has entries.absolute_path, require a rebuild.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(entries);`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, "absolute_path") {
			return fmt.Errorf("unsupported database schema: entries.absolute_path exists; delete the DB file and reindex to rebuild")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_entries_index_id ON entries(index_id);
	`); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE UNIQUE INDEX IF NOT EXISTS idx_entries_path ON entries(index_id, relative_path);
	`); err != nil {
		return err
	}

	if err := ensureColumn(ctx, db, "entries", "path_depth", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "entries", "inode", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "entries", "last_seen", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "indexes", "index_duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "indexes", "export_duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := ensureColumn(ctx, db, "indexes", "vacuum_duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// Backfill path_depth for existing rows (0 for root "/", otherwise number of segments)
	// Safe to run on every startup; it only updates rows still at the default value.
	if _, err := db.ExecContext(ctx, `
		UPDATE entries
		SET path_depth = (LENGTH(relative_path) - LENGTH(REPLACE(relative_path, '/', '')))
		WHERE relative_path != '/' AND path_depth = 0;
	`); err != nil {
		return err
	}

	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_entries_subfolders ON entries(index_id, type, path_depth, relative_path);
	`); err != nil {
		return err
	}

	return nil
}

func ensureColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	query := fmt.Sprintf(`PRAGMA table_info(%s);`, table)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultVal, &pk); err != nil {
			return err
		}
		if strings.EqualFold(name, column) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	stmt := fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s;`, table, column, definition)
	_, err = db.ExecContext(ctx, stmt)
	return err
}

func insertEntriesBatch(ctx context.Context, tx *sql.Tx, indexID int64, scanTime int64, batch []indexing.IndexEntry) error {
	if len(batch) == 0 {
		return nil
	}

	const insertPrefix = `
INSERT INTO entries (
	index_id,
	relative_path,
	path_depth,
	name,
	size,
	mod_time,
	type,
	hidden,
	inode,
	last_seen
) VALUES `
	const singlePlaceholder = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	const upsertSuffix = `
ON CONFLICT(index_id, relative_path) DO UPDATE SET
	name = excluded.name,
	size = excluded.size,
	mod_time = excluded.mod_time,
	type = excluded.type,
	hidden = excluded.hidden,
	inode = excluded.inode,
	last_seen = excluded.last_seen;
`

	var builder strings.Builder
	builder.Grow(len(insertPrefix) + len(singlePlaceholder)*len(batch) + len(batch) + len(upsertSuffix))
	builder.WriteString(insertPrefix)

	args := make([]any, 0, len(batch)*10)
	for i, entry := range batch {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(singlePlaceholder)
		pathDepth := 0
		if entry.RelativePath != "/" {
			pathDepth = strings.Count(entry.RelativePath, "/")
		}
		args = append(args,
			indexID,
			entry.RelativePath,
			pathDepth,
			entry.Name,
			entry.Size,
			entry.ModTime.Unix(),
			entry.Type,
			indexing.BoolToInt(entry.Hidden),
			int64(entry.Inode),
			scanTime,
		)
	}

	builder.WriteString(upsertSuffix)

	_, err := tx.ExecContext(ctx, builder.String(), args...)
	return err
}

func ensureContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func parentDirKey(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		return "/"
	}
	parent := filepath.Dir(trimmed)
	if parent == "." || parent == "" {
		return "/"
	}
	if parent == "/" {
		return "/"
	}
	return "/" + strings.Trim(parent, "/")
}

// UpdateEntry updates or inserts a single entry in the database.
// Returns the old size if the entry existed, or 0 if it's new.
func UpdateEntry(ctx context.Context, db dbExecutor, indexID int64, entry indexing.IndexEntry) (oldSize int64, err error) {
	ctx = ensureContext(ctx)

	// Check if entry already exists and get old size
	var existingSize sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT size FROM entries
		WHERE index_id = ? AND relative_path = ?;
	`, indexID, entry.RelativePath).Scan(&existingSize)

	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}

	if existingSize.Valid {
		oldSize = existingSize.Int64
	}

	// Upsert the entry
	pathDepth := 0
	if entry.RelativePath != "/" {
		pathDepth = strings.Count(entry.RelativePath, "/")
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO entries (
			index_id, relative_path, path_depth, name, size, mod_time,
			type, hidden, inode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(index_id, relative_path) DO UPDATE SET
			name = excluded.name,
			size = excluded.size,
			mod_time = excluded.mod_time,
			type = excluded.type,
			hidden = excluded.hidden,
			inode = excluded.inode;
	`,
		indexID,
		entry.RelativePath,
		pathDepth,
		entry.Name,
		entry.Size,
		entry.ModTime.Unix(),
		entry.Type,
		indexing.BoolToInt(entry.Hidden),
		int64(entry.Inode),
	)

	return oldSize, err
}

// DeleteEntry removes an entry from the database and returns its size.
func DeleteEntry(ctx context.Context, db dbExecutor, indexID int64, relativePath string) (oldSize int64, err error) {
	ctx = ensureContext(ctx)

	// Get the size before deleting
	var size sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT size FROM entries
		WHERE index_id = ? AND relative_path = ?;
	`, indexID, relativePath).Scan(&size)

	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil // Entry doesn't exist
		}
		return 0, err
	}

	if size.Valid {
		oldSize = size.Int64
	}

	// Delete the entry
	_, err = db.ExecContext(ctx, `
		DELETE FROM entries WHERE index_id = ? AND relative_path = ?;
	`, indexID, relativePath)

	return oldSize, err
}

// UpdateParentDirectorySizes propagates size changes up the directory tree.
// sizeDelta is the change in size (positive for additions, negative for deletions).
func UpdateParentDirectorySizes(ctx context.Context, db dbExecutor, indexID int64, childPath string, sizeDelta int64) error {
	ctx = ensureContext(ctx)
	if sizeDelta == 0 {
		return nil
	}

	currentPath := parentDirKey(childPath)

	// Walk up the directory tree
	for currentPath != "" {
		_, err := db.ExecContext(ctx, `
			UPDATE entries
			SET size = size + ?
			WHERE index_id = ? AND relative_path = ? AND type = 'directory';
		`, sizeDelta, indexID, currentPath)

		if err != nil {
			return err
		}

		// Move to parent
		if currentPath == "/" {
			break
		}
		currentPath = parentDirKey(currentPath)
	}

	return nil
}

// UpsertEntryWithSizeUpdate updates an entry and propagates size changes to parents.
// This is a convenience function that combines UpdateEntry and UpdateParentDirectorySizes.
func UpsertEntryWithSizeUpdate(ctx context.Context, db *sql.DB, indexID int64, entry indexing.IndexEntry) error {
	ctx = ensureContext(ctx)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	oldSize, err := UpdateEntry(ctx, tx, indexID, entry)
	if err != nil {
		return err
	}

	sizeDelta := entry.Size - oldSize
	if sizeDelta != 0 {
		if err := UpdateParentDirectorySizes(ctx, tx, indexID, entry.RelativePath, sizeDelta); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeleteEntryWithSizeUpdate deletes an entry and propagates size changes to parents.
func DeleteEntryWithSizeUpdate(ctx context.Context, db *sql.DB, indexID int64, relativePath string) error {
	ctx = ensureContext(ctx)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	oldSize, err := DeleteEntry(ctx, tx, indexID, relativePath)
	if err != nil {
		return err
	}

	if oldSize != 0 {
		if err := UpdateParentDirectorySizes(ctx, tx, indexID, relativePath, -oldSize); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// DeletePathRecursive deletes all entries under a path (including the path itself) and propagates size changes.
func DeletePathRecursive(ctx context.Context, db *sql.DB, indexID int64, relativePath string) error {
	ctx = ensureContext(ctx)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Calculate total size of all entries being deleted
	var totalSize int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size), 0)
		FROM entries
		WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?);
	`, indexID, relativePath, relativePath+"/%").Scan(&totalSize)
	if err != nil {
		return err
	}

	// Delete all entries under this path (including the path itself)
	_, err = tx.ExecContext(ctx, `
		DELETE FROM entries
		WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?);
	`, indexID, relativePath, relativePath+"/%")
	if err != nil {
		return err
	}

	// Propagate size changes to parent directories
	if totalSize != 0 {
		if err := UpdateParentDirectorySizes(ctx, tx, indexID, relativePath, -totalSize); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// CleanupDeletedEntries removes entries that were not seen during the latest scan.
// Returns the number of entries deleted.
func CleanupDeletedEntries(ctx context.Context, db *sql.DB, indexID int64, scanTime int64) (int64, error) {
	ctx = ensureContext(ctx)

	result, err := db.ExecContext(ctx, `
		DELETE FROM entries
		WHERE index_id = ? AND last_seen < ?;
	`, indexID, scanTime)
	if err != nil {
		return 0, err
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	return deleted, nil
}

// CleanupDeletedEntriesUnderPath removes entries under a specific path that were not seen during the latest scan.
// This is used for partial reindexing to avoid deleting entries outside the reindexed path.
// Returns the number of entries deleted.
func CleanupDeletedEntriesUnderPath(ctx context.Context, db *sql.DB, indexID int64, relativePath string, scanTime int64) (int64, error) {
	ctx = ensureContext(ctx)

	result, err := db.ExecContext(ctx, `
		DELETE FROM entries
		WHERE index_id = ?
		  AND last_seen < ?
		  AND (relative_path = ? OR relative_path LIKE ?);
	`, indexID, scanTime, relativePath, relativePath+"/%")
	if err != nil {
		return 0, err
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	return deleted, nil
}

// ReleaseSQLiteMemory forces SQLite to release cached memory.
// Call this after heavy write operations to return memory to the OS.
func ReleaseSQLiteMemory(ctx context.Context, db *sql.DB) error {
	ctx = ensureContext(ctx)

	// Shrink SQLite's page cache
	if _, err := db.ExecContext(ctx, `PRAGMA shrink_memory;`); err != nil {
		logger.Warnf("Failed to shrink SQLite memory: %v", err)
	}

	// Optimize database (lightweight, doesn't rebuild like VACUUM)
	if _, err := db.ExecContext(ctx, `PRAGMA optimize;`); err != nil {
		logger.Warnf("Failed to optimize SQLite: %v", err)
	}

	return nil
}
