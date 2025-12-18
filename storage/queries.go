package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/indexing"
)

// Store wraps the database connection
type Store struct {
	db     *sql.DB
	dbPath string
}

var ErrDirectoryNotFound = errors.New("directory not found")

// NewStore creates a new Store instance and owns the DB handle.
func NewStore(dbPath string) (*Store, error) {
	db, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, dbPath: dbPath}, nil
}

// NewStoreWithDB reuses an existing database handle (e.g., long-lived server).
// dbPath should be the actual SQLite file path (for stats / size reporting).
func NewStoreWithDB(db *sql.DB, dbPath string) *Store {
	return &Store{db: db, dbPath: dbPath}
}

// Close closes the database connection (only use if Store owns the DB).
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying database handle for reuse (e.g., long-lived servers).
func (s *Store) DB() *sql.DB {
	return s.db
}

// LatestIndexID returns the ID of the most recently indexed index.
// - sql.ErrNoRows means "no indexes yet"
// - Any other error is a real DB problem and should be surfaced.
func (s *Store) LatestIndexID(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var id int64
	err := s.db.QueryRowContext(ctx, `
        SELECT id
        FROM indexes
        ORDER BY last_indexed DESC
        LIMIT 1
    `).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// EntryResult represents a query result entry
type EntryResult struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"` // "folder" or "file"
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mod_time"`
	Inode      uint64    `json:"inode"`
	TotalSize  int64     `json:"total_size,omitempty"`
	TotalFiles int64     `json:"total_files,omitempty"`
	TotalDirs  int64     `json:"total_dirs,omitempty"`
}

// SearchEntries performs a name search against the latest index.
func (s *Store) SearchEntries(ctx context.Context, pattern string, limit int) ([]EntryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 100
	}

	indexID, err := s.LatestIndexID(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No index yet → no results, not an error
			return []EntryResult{}, nil
		}
		return nil, fmt.Errorf("failed to get latest index: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
        SELECT relative_path, name, type, size, mod_time, inode
        FROM entries
        WHERE index_id = ? AND name LIKE ?
        ORDER BY mod_time DESC
        LIMIT ?
    `, indexID, "%"+pattern+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			logger.Warnf("rows close (search): %v", cerr)
		}
	}()

	var results []EntryResult
	for rows.Next() {
		var entry EntryResult
		var modUnix int64
		var dbType string
		if err := rows.Scan(&entry.Path, &entry.Name, &dbType, &entry.Size, &modUnix, &entry.Inode); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		// Convert database type to API type
		if dbType == "directory" {
			entry.Type = "folder"
		} else {
			entry.Type = "file"
		}
		entry.ModTime = time.Unix(modUnix, 0)
		results = append(results, entry)
	}
	return results, rows.Err()
}

// DirSize returns the pre-calculated size for a directory.
// The size is calculated during indexing and stored in the directory entry.
func (s *Store) DirSize(ctx context.Context, path string) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	indexID, err := s.LatestIndexID(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil // no index yet
		}
		return 0, fmt.Errorf("failed to get latest index: %w", err)
	}

	var size sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
        SELECT size
        FROM entries
        WHERE index_id = ? AND relative_path = ? AND type = 'directory'
    `, indexID, path).Scan(&size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s", ErrDirectoryNotFound, path)
		}
		return 0, fmt.Errorf("dir size query failed: %w", err)
	}
	if size.Valid {
		return size.Int64, nil
	}
	return 0, nil
}

// UpsertEntry inserts or replaces a single entry (used for manual updates via API).
// Path should be the relative path (e.g., "/dir/file.txt"), absPath is the full filesystem path.
func (s *Store) UpsertEntry(indexID int64, entry EntryResult, absPath, typ string, hidden bool) error {
	ctx := context.Background()
	pathDepth := 0
	if entry.Path != "/" {
		pathDepth = strings.Count(entry.Path, "/")
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO entries (
            index_id, relative_path, path_depth, name, size, mod_time, type, hidden, inode
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(index_id, relative_path) DO UPDATE SET
            name=excluded.name,
            size=excluded.size,
            mod_time=excluded.mod_time,
            type=excluded.type,
            hidden=excluded.hidden,
            inode=excluded.inode;
    `, indexID, entry.Path, pathDepth, entry.Name, entry.Size, entry.ModTime.Unix(), typ, indexing.BoolToInt(hidden), entry.Inode)
	return err
}

// QueryPath queries entries at or under a given path
func (s *Store) QueryPath(ctx context.Context, path string, recursive bool, limit, offset int) ([]EntryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	indexID, err := s.LatestIndexID(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []EntryResult{}, nil
		}
		return nil, fmt.Errorf("failed to get latest index: %w", err)
	}

	var query string
	var args []any

	if recursive {
		query = `
            SELECT relative_path, name, type, size, mod_time, inode
            FROM entries
            WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?)
            ORDER BY relative_path
        `
		args = []any{indexID, path, path + "%"}
	} else {
		query = `
            SELECT relative_path, name, type, size, mod_time, inode
            FROM entries
            WHERE index_id = ? AND relative_path = ?
        `
		args = []any{indexID, path}
	}

	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
		if offset > 0 {
			query += fmt.Sprintf(" OFFSET %d", offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			logger.Warnf("rows close (query): %v", cerr)
		}
	}()

	var results []EntryResult
	for rows.Next() {
		var entry EntryResult
		var modUnix int64
		var dbType string

		err := rows.Scan(&entry.Path, &entry.Name, &dbType, &entry.Size, &modUnix, &entry.Inode)
		if err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}

		// Convert database type to API type
		if dbType == "directory" {
			entry.Type = "folder"
		} else {
			entry.Type = "file"
		}
		entry.ModTime = time.Unix(modUnix, 0)
		results = append(results, entry)
	}

	return results, rows.Err()
}

// Stats represents database statistics
type Stats struct {
	TotalIndexes int       `json:"total_indexes"`
	TotalEntries int64     `json:"total_entries"`
	TotalSize    int64     `json:"total_size"`
	LastScanTime time.Time `json:"last_scan_time"`
	DatabaseSize int64     `json:"database_size"` // main DB file only (backwards-compatible)
	WALSize      int64     `json:"wal_size"`
	SHMSize      int64     `json:"shm_size"`
	TotalOnDisk  int64     `json:"total_on_disk"`
}

// GetStats returns database statistics
func (s *Store) GetStats(ctx context.Context) (*Stats, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	var stats Stats

	// Get total indexes
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM indexes`).Scan(&stats.TotalIndexes)
	if err != nil {
		return nil, err
	}

	// Get total entries and size and last_indexed
	var lastIndexed sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
        SELECT
            COALESCE(SUM(num_files + num_dirs), 0),
            COALESCE(SUM(total_size), 0),
            MAX(last_indexed)
        FROM indexes
    `).Scan(&stats.TotalEntries, &stats.TotalSize, &lastIndexed)
	if err != nil {
		return nil, err
	}

	if lastIndexed.Valid {
		stats.LastScanTime = time.Unix(lastIndexed.Int64, 0)
	}

	// Get database file size using the actual dbPath
	if s.dbPath != "" {
		if fi, err := os.Stat(s.dbPath); err == nil {
			stats.DatabaseSize = fi.Size()
		}
		if fi, err := os.Stat(s.dbPath + "-wal"); err == nil {
			stats.WALSize = fi.Size()
		}
		if fi, err := os.Stat(s.dbPath + "-shm"); err == nil {
			stats.SHMSize = fi.Size()
		}
		stats.TotalOnDisk = stats.DatabaseSize + stats.WALSize + stats.SHMSize
	}

	return &stats, nil
}

// UpsertEntryWithSizeUpdate updates an entry and propagates size changes to parents.
func (s *Store) UpsertEntryWithSizeUpdate(ctx context.Context, indexID int64, entry indexing.IndexEntry) error {
	return UpsertEntryWithSizeUpdate(ctx, s.db, indexID, entry)
}

// DeleteEntryWithSizeUpdate deletes an entry and propagates size changes to parents.
func (s *Store) DeleteEntryWithSizeUpdate(ctx context.Context, indexID int64, relativePath string) error {
	return DeleteEntryWithSizeUpdate(ctx, s.db, indexID, relativePath)
}

// SubfolderResult represents a direct subfolder with its size
type SubfolderResult struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// GetDirectSubfolders returns all direct child folders of a given path with their sizes.
// This only returns immediate children (not recursive).
func (s *Store) GetDirectSubfolders(ctx context.Context, parentPath string) ([]SubfolderResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	indexID, err := s.LatestIndexID(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []SubfolderResult{}, nil
		}
		return nil, fmt.Errorf("failed to get latest index: %w", err)
	}

	// Normalize parent path - ensure it starts with / and doesn't end with / (unless it's root)
	if parentPath != "/" {
		parentPath = "/" + strings.Trim(parentPath, "/")
	}

	parentDepth := 0
	if parentPath != "/" {
		parentDepth = strings.Count(parentPath, "/")
	}
	childDepth := parentDepth + 1

	// Query for direct children that are directories using path_depth (precomputed at write time).
	var (
		query string
		args  []any
	)

	if parentPath == "/" {
		query = `
            SELECT relative_path, name, size, mod_time
            FROM entries
            WHERE index_id = ?
              AND type = 'directory'
              AND path_depth = 1
            ORDER BY name
        `
		args = []any{indexID}
	} else {
		query = `
            SELECT relative_path, name, size, mod_time
            FROM entries
            WHERE index_id = ?
              AND type = 'directory'
              AND path_depth = ?
              AND relative_path LIKE ?
            ORDER BY name
        `
		args = []any{indexID, childDepth, parentPath + "/%"}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query subfolders failed: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil {
			logger.Warnf("rows close (subfolders): %v", cerr)
		}
	}()

	var results []SubfolderResult
	for rows.Next() {
		var subfolder SubfolderResult
		var modUnix int64
		if err := rows.Scan(&subfolder.Path, &subfolder.Name, &subfolder.Size, &modUnix); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		subfolder.ModTime = time.Unix(modUnix, 0)
		results = append(results, subfolder)
	}

	return results, rows.Err()
}
