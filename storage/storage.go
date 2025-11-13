package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"indexer/indexing"
	"indexer/indexing/iteminfo"
)

const (
	defaultDBPath = "indexer.db"
	busyTimeoutMS = 5000
	schemaTimeout = 30 * time.Second
)

var ErrSnapshotNotFound = errors.New("snapshot not found")

// PersistMetrics captures duration measurements for the indexing workflow.
type PersistMetrics struct {
	IndexDuration  time.Duration
	ExportDuration time.Duration
	VacuumDuration time.Duration
}

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
func SaveIndex(ctx context.Context, db *sql.DB, idx *indexing.Index, metrics *PersistMetrics) error {
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

// LoadSnapshotIntoIndex loads the previously persisted directory tree for the
// given index name (if available) and seeds the provided Index for quick scans.
func LoadSnapshotIntoIndex(ctx context.Context, path, name string, idx *indexing.Index) error {
	if idx == nil {
		return fmt.Errorf("storage: index cannot be nil")
	}
	dirs, err := loadSnapshotDirectories(ctx, path, name)
	if err != nil {
		return err
	}
	if len(dirs) == 0 {
		return ErrSnapshotNotFound
	}
	idx.ApplySnapshot(dirs)
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

func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

// ListIndexNames returns all index names stored in the given SQLite database.
// The path semantics match other helpers in this package: when empty, the
// default DB path is used.
func ListIndexNames(ctx context.Context, path string) ([]string, error) {
	if path == "" {
		path = defaultDBPath
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d&_foreign_keys=on", absPath, busyTimeoutMS)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	if ctx == nil {
		ctx = context.Background()
	}

	rows, err := db.QueryContext(ctx, `SELECT name FROM indexes ORDER BY name;`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, ErrSnapshotNotFound
	}
	return names, nil
}

// DirStats represents aggregate information about a directory inside an index.
type DirStats struct {
	RelativePath string
	Size         int64
	ModTime      time.Time
	NumDirs      int64
	NumFiles     int64
}

// GetDirStats returns aggregate size, modification time, and recursive file/dir
// counts for a given directory path inside an index. The relPath can be any
// path that was originally indexed under the root (e.g. "/", "/var/log",
// "/home/user/docs"); it is normalized to the same format used by the
// indexer when exporting entries.
func GetDirStats(ctx context.Context, path, name, relPath string) (*DirStats, error) {
	// Normalize to the same representation as indexing.normalizeRelativePath:
	// - root => "/"
	// - other paths => leading "/" and no trailing "/"
	if relPath == "" || relPath == "." || relPath == "/" {
		relPath = "/"
	} else {
		if !strings.HasPrefix(relPath, "/") {
			relPath = "/" + relPath
		}
		relPath = "/" + strings.Trim(relPath, "/")
	}

	if path == "" {
		path = defaultDBPath
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d&_foreign_keys=on", absPath, busyTimeoutMS)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	if ctx == nil {
		ctx = context.Background()
	}

	var indexID int64
	scanErr := db.QueryRowContext(ctx, `SELECT id FROM indexes WHERE name = ?`, name).Scan(&indexID)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, ErrSnapshotNotFound
	}
	if scanErr != nil {
		return nil, scanErr
	}

	var size, modUnix int64
	row := db.QueryRowContext(ctx, `
		SELECT size, mod_time
		FROM entries
		WHERE index_id = ? AND is_dir = 1 AND relative_path = ?;
	`, indexID, relPath)
	if err := row.Scan(&size, &modUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}

	// Count directories and files recursively under this path.
	var likePrefix string
	if relPath == "/" {
		likePrefix = "/%"
	} else {
		likePrefix = relPath + "/%"
	}

	var numDirs, numFiles int64
	row = db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM entries
		WHERE index_id = ? AND is_dir = 1 AND relative_path LIKE ? AND relative_path <> ?;
	`, indexID, likePrefix, relPath)
	if err := row.Scan(&numDirs); err != nil {
		return nil, err
	}

	row = db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM entries
		WHERE index_id = ? AND is_dir = 0 AND relative_path LIKE ?;
	`, indexID, likePrefix)
	if err := row.Scan(&numFiles); err != nil {
		return nil, err
	}

	return &DirStats{
		RelativePath: relPath,
		Size:         size,
		ModTime:      time.Unix(modUnix, 0).UTC(),
		NumDirs:      numDirs,
		NumFiles:     numFiles,
	}, nil
}

func loadSnapshotDirectories(ctx context.Context, path, name string) (map[string]*iteminfo.FileInfo, error) {
	if path == "" {
		path = defaultDBPath
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(absPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrSnapshotNotFound
		}
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d&_foreign_keys=on", absPath, busyTimeoutMS)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	if ctx == nil {
		ctx = context.Background()
	}

	var indexID int64
	scanErr := db.QueryRowContext(ctx, `SELECT id FROM indexes WHERE name = ?`, name).Scan(&indexID)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, ErrSnapshotNotFound
	}
	if scanErr != nil {
		return nil, scanErr
	}

	dirs, err := fetchDirectoryEntries(ctx, db, indexID)
	if err != nil {
		return nil, err
	}
	if len(dirs) == 0 {
		return nil, ErrSnapshotNotFound
	}
	if err := attachChildren(ctx, db, indexID, dirs); err != nil {
		return nil, err
	}
	return dirs, nil
}

func fetchDirectoryEntries(ctx context.Context, db *sql.DB, indexID int64) (map[string]*iteminfo.FileInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT relative_path, name, size, mod_time, hidden, inode
		FROM entries
		WHERE index_id = ? AND is_dir = 1;
	`, indexID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	dirs := make(map[string]*iteminfo.FileInfo)
	for rows.Next() {
		var relPath, name string
		var size, modUnix, inode int64
		var hiddenInt int
		if err := rows.Scan(&relPath, &name, &size, &modUnix, &hiddenInt, &inode); err != nil {
			return nil, err
		}
		key := dirMapKey(relPath)
		info := &iteminfo.FileInfo{
			Path: key,
		}
		info.ItemInfo = iteminfo.ItemInfo{
			Name:    name,
			Size:    size,
			ModTime: time.Unix(modUnix, 0).UTC(),
			Type:    "directory",
			Hidden:  hiddenInt == 1,
			Inode:   uint64(inode),
		}
		dirs[key] = info
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return dirs, nil
}

func attachChildren(ctx context.Context, db *sql.DB, indexID int64, dirs map[string]*iteminfo.FileInfo) error {
	// attach folders
	for path, info := range dirs {
		parent := parentDirKey(path)
		if parent == "" || parent == path {
			continue
		}
		if parentInfo, ok := dirs[parent]; ok {
			parentInfo.Folders = append(parentInfo.Folders, info.ItemInfo)
		}
	}

	rows, err := db.QueryContext(ctx, `
		SELECT relative_path, name, size, mod_time, hidden, inode
		FROM entries
		WHERE index_id = ? AND is_dir = 0;
	`, indexID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var relPath, name string
		var size, modUnix, inode int64
		var hiddenInt int
		if err := rows.Scan(&relPath, &name, &size, &modUnix, &hiddenInt, &inode); err != nil {
			return err
		}
		parent := parentDirKey(dirMapKey(relPath))
		if parent == "" {
			parent = "/"
		}
		parentInfo, ok := dirs[parent]
		if !ok {
			continue
		}
		item := iteminfo.ItemInfo{
			Name:    name,
			Size:    size,
			ModTime: time.Unix(modUnix, 0).UTC(),
			Type:    "file",
			Hidden:  hiddenInt == 1,
			Inode:   uint64(inode),
		}
		parentInfo.Files = append(parentInfo.Files, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, info := range dirs {
		info.SortItems()
	}
	return nil
}

func dirMapKey(relPath string) string {
	if relPath == "" || relPath == "/" {
		return "/"
	}
	clean := "/" + strings.Trim(relPath, "/")
	if clean == "/" {
		return "/"
	}
	return clean + "/"
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
	return "/" + strings.Trim(parent, "/") + "/"
}
