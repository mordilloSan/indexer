package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mordilloSan/go_logger/logger"

	"indexer/indexing"
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
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// PersistMetrics captures duration measurements for the indexing workflow.
type PersistMetrics struct {
	IndexDuration  time.Duration
	ExportDuration time.Duration
	VacuumDuration time.Duration
}

// StreamingWriter accepts entries via a channel and writes them to the database in batches.
type StreamingWriter struct {
	db       *sql.DB
	indexID  int64
	scanTime int64
	entryCh  chan indexing.IndexEntry
	doneCh   chan error
	ctx      context.Context
	cancel   context.CancelFunc
	errVal   atomic.Value
}

// NewStreamingWriter creates a writer that batches entries and commits periodically.
func NewStreamingWriter(db *sql.DB, indexID int64, bufferSize int) *StreamingWriter {
	if bufferSize <= 0 {
		bufferSize = 1000
	}
	ctx, cancel := context.WithCancel(context.Background())
	sw := &StreamingWriter{
		db:       db,
		indexID:  indexID,
		scanTime: time.Now().UTC().Unix(),
		entryCh:  make(chan indexing.IndexEntry, bufferSize),
		doneCh:   make(chan error, 1),
		ctx:      ctx,
		cancel:   cancel,
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
	dsn := fmt.Sprintf("%s?_busy_timeout=%d&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL", path, busyTimeoutMS)
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
	db.SetMaxOpenConns(5)    // Allow up to 5 concurrent connections
	db.SetMaxIdleConns(2)    // Keep 2 connections ready
	db.SetConnMaxLifetime(0) // Reuse connections indefinitely

	if err := initSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	logger.Infof("Database connected: %s", path)
	logger.Infof("Database journal_mode: %s", strings.ToUpper(journalMode))

	return db, nil
}

// SaveIndex stores the indexed data inside a single transaction.
func SaveIndex(ctx context.Context, db *sql.DB, idx *indexing.Index, metrics *PersistMetrics) error {
	if db == nil || idx == nil {
		return fmt.Errorf("storage: db and index must be provided")
	}
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

	var indexID int64
	indexID, err = upsertIndex(ctx, tx, idx, metrics)
	if err != nil {
		return err
	}

	err = replaceEntries(ctx, tx, indexID, idx.ExportEntries())
	if err != nil {
		return err
	}

	err = tx.Commit()
	return err
}

// SaveIndexToFile persists the index into an on-disk SQLite database by first writing
// everything into an in-memory database and then exporting it via VACUUM INTO.
func SaveIndexToFile(ctx context.Context, path string, idx *indexing.Index, metrics *PersistMetrics) error {
	ctx = ensureContext(ctx)
	if path == "" {
		path = defaultDBPath
	}

	memDB, err := openMemoryDB()
	if err != nil {
		return err
	}
	defer func() { _ = memDB.Close() }()

	if err := initSchema(ctx, memDB); err != nil {
		return err
	}

	exportStart := time.Now()
	if err := SaveIndex(ctx, memDB, idx, metrics); err != nil {
		return err
	}
	if metrics != nil {
		metrics.ExportDuration = time.Since(exportStart)
	}

	vacuumStart := time.Now()
	if err := flushToDisk(ctx, memDB, path); err != nil {
		return err
	}
	if metrics != nil {
		metrics.VacuumDuration = time.Since(vacuumStart)
	}

	return nil
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
			absolute_path TEXT NOT NULL,
			name TEXT NOT NULL,
			size INTEGER NOT NULL,
			mod_time INTEGER NOT NULL,
			type TEXT NOT NULL,
			hidden INTEGER NOT NULL DEFAULT 0,
			is_dir INTEGER NOT NULL DEFAULT 0,
			inode INTEGER NOT NULL DEFAULT 0,
			last_seen INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (index_id) REFERENCES indexes(id) ON DELETE CASCADE
		);
	`); err != nil {
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

	return nil
}

func upsertIndex(ctx context.Context, tx *sql.Tx, idx *indexing.Index, metrics *PersistMetrics) (int64, error) {
	now := time.Now().UTC().Unix()
	var indexDuration, exportDuration, vacuumDuration int64
	if metrics != nil {
		indexDuration = durationMillis(metrics.IndexDuration)
		exportDuration = durationMillis(metrics.ExportDuration)
		vacuumDuration = durationMillis(metrics.VacuumDuration)
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO indexes (
			name, root_path, source, include_hidden,
			num_dirs, num_files, total_size, disk_used,
			disk_total, last_indexed,
			index_duration_ms, export_duration_ms, vacuum_duration_ms
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			root_path=excluded.root_path,
			source=excluded.source,
			include_hidden=excluded.include_hidden,
			num_dirs=excluded.num_dirs,
			num_files=excluded.num_files,
			total_size=excluded.total_size,
			disk_used=excluded.disk_used,
			disk_total=excluded.disk_total,
			last_indexed=excluded.last_indexed,
			index_duration_ms=excluded.index_duration_ms,
			export_duration_ms=excluded.export_duration_ms,
			vacuum_duration_ms=excluded.vacuum_duration_ms;
	`,
		idx.Name,
		idx.Path,
		idx.Source,
		boolToInt(idx.IncludeHidden()),
		idx.NumDirs,
		idx.NumFiles,
		int64(idx.GetTotalSize()),
		int64(idx.DiskUsed),
		int64(idx.DiskTotal),
		now,
		indexDuration,
		exportDuration,
		vacuumDuration,
	)
	if err != nil {
		return 0, err
	}

	var indexID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM indexes WHERE name = ?;`, idx.Name).Scan(&indexID); err != nil {
		return 0, err
	}

	return indexID, nil
}

func replaceEntries(ctx context.Context, tx *sql.Tx, indexID int64, entries []indexing.IndexEntry) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE index_id = ?;`, indexID); err != nil {
		return err
	}

	if len(entries) == 0 {
		return nil
	}

	scanTime := time.Now().UTC().Unix()
	const batchSize = 500
	for start := 0; start < len(entries); start += batchSize {
		end := start + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := insertEntriesBatch(ctx, tx, indexID, scanTime, entries[start:end]); err != nil {
			return err
		}
	}

	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
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
	absolute_path,
	name,
	size,
	mod_time,
	type,
	hidden,
	is_dir,
	inode,
	last_seen
) VALUES `
	const singlePlaceholder = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	const upsertSuffix = `
ON CONFLICT(index_id, relative_path) DO UPDATE SET
	absolute_path = excluded.absolute_path,
	name = excluded.name,
	size = excluded.size,
	mod_time = excluded.mod_time,
	type = excluded.type,
	hidden = excluded.hidden,
	is_dir = excluded.is_dir,
	inode = excluded.inode,
	last_seen = excluded.last_seen;
`

	var builder strings.Builder
	builder.Grow(len(insertPrefix) + len(singlePlaceholder)*len(batch) + len(batch) + len(upsertSuffix))
	builder.WriteString(insertPrefix)

	args := make([]any, 0, len(batch)*11)
	for i, entry := range batch {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(singlePlaceholder)
		args = append(args,
			indexID,
			entry.RelativePath,
			entry.AbsolutePath,
			entry.Name,
			entry.Size,
			entry.ModTime.Unix(),
			entry.Type,
			boolToInt(entry.Hidden),
			boolToInt(entry.IsDir),
			int64(entry.Inode),
			scanTime,
		)
	}

	builder.WriteString(upsertSuffix)

	_, err := tx.ExecContext(ctx, builder.String(), args...)
	return err
}

func openMemoryDB() (*sql.DB, error) {
	dsn := fmt.Sprintf("file:indexer_mem?mode=memory&cache=shared&_busy_timeout=%d&_foreign_keys=on", busyTimeoutMS)
	return sql.Open("sqlite3", dsn)
}

func flushToDisk(ctx context.Context, db *sql.DB, path string) error {
	ctx = ensureContext(ctx)
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(absPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := os.Remove(absPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	stmt := fmt.Sprintf("VACUUM INTO %s;", quoteLiteral(absPath))
	_, err = db.ExecContext(ctx, stmt)
	return err
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
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
	_, err = db.ExecContext(ctx, `
		INSERT INTO entries (
			index_id, relative_path, absolute_path, name, size, mod_time,
			type, hidden, is_dir, inode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(index_id, relative_path) DO UPDATE SET
			absolute_path = excluded.absolute_path,
			name = excluded.name,
			size = excluded.size,
			mod_time = excluded.mod_time,
			type = excluded.type,
			hidden = excluded.hidden,
			is_dir = excluded.is_dir,
			inode = excluded.inode;
	`,
		indexID,
		entry.RelativePath,
		entry.AbsolutePath,
		entry.Name,
		entry.Size,
		entry.ModTime.Unix(),
		entry.Type,
		boolToInt(entry.Hidden),
		boolToInt(entry.IsDir),
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
			WHERE index_id = ? AND relative_path = ? AND is_dir = 1;
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
