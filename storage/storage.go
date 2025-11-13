package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"indexer/indexing"
)

const (
	defaultDBPath = "indexer.db"
	busyTimeoutMS = 5000
	schemaTimeout = 30 * time.Second
)

// Open creates (or reuses) a SQLite database and ensures the schema exists.
func Open(path string) (*sql.DB, error) {
	if path == "" {
		path = defaultDBPath
	}
	dsn := fmt.Sprintf("%s?_busy_timeout=%d&_foreign_keys=on", path, busyTimeoutMS)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaTimeout)
	defer cancel()

	if err := initSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// SaveIndex stores the indexed data inside a single transaction.
func SaveIndex(ctx context.Context, db *sql.DB, idx *indexing.Index) error {
	if db == nil || idx == nil {
		return fmt.Errorf("storage: db and index must be provided")
	}

	if ctx == nil {
		ctx = context.Background()
	}

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
	indexID, err = upsertIndex(ctx, tx, idx)
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
func SaveIndexToFile(ctx context.Context, path string, idx *indexing.Index) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	if err := SaveIndex(ctx, memDB, idx); err != nil {
		return err
	}

	return flushToDisk(ctx, memDB, path)
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
		CREATE INDEX IF NOT EXISTS idx_entries_path ON entries(index_id, relative_path);
	`); err != nil {
		return err
	}

	if err := ensureColumn(ctx, db, "entries", "inode", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	return nil
}

func upsertIndex(ctx context.Context, tx *sql.Tx, idx *indexing.Index) (int64, error) {
	now := time.Now().UTC().Unix()

	_, err := tx.ExecContext(ctx, `
		INSERT INTO indexes (
			name, root_path, source, include_hidden,
			num_dirs, num_files, total_size, disk_used,
			disk_total, last_indexed
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			root_path=excluded.root_path,
			source=excluded.source,
			include_hidden=excluded.include_hidden,
			num_dirs=excluded.num_dirs,
			num_files=excluded.num_files,
			total_size=excluded.total_size,
			disk_used=excluded.disk_used,
			disk_total=excluded.disk_total,
			last_indexed=excluded.last_indexed;
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

	const batchSize = 500
	for start := 0; start < len(entries); start += batchSize {
		end := start + batchSize
		if end > len(entries) {
			end = len(entries)
		}
		if err := insertEntriesBatch(ctx, tx, indexID, entries[start:end]); err != nil {
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

func insertEntriesBatch(ctx context.Context, tx *sql.Tx, indexID int64, batch []indexing.IndexEntry) error {
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
	inode
) VALUES `
	const singlePlaceholder = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

	var builder strings.Builder
	builder.Grow(len(insertPrefix) + len(singlePlaceholder)*len(batch) + len(batch))
	builder.WriteString(insertPrefix)

	args := make([]any, 0, len(batch)*10)
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
		)
	}

	_, err := tx.ExecContext(ctx, builder.String(), args...)
	return err
}

func openMemoryDB() (*sql.DB, error) {
	dsn := fmt.Sprintf("file:indexer_mem?mode=memory&cache=shared&_busy_timeout=%d&_foreign_keys=on", busyTimeoutMS)
	return sql.Open("sqlite3", dsn)
}

func flushToDisk(ctx context.Context, db *sql.DB, path string) error {
	if ctx == nil {
		ctx = context.Background()
	}

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
