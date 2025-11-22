package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/mordilloSan/go_logger/logger"
)

// Store wraps the database connection
type Store struct {
	db *sql.DB
}

// NewStore creates a new Store instance
func NewStore(dbPath string) (*Store, error) {
	db, err := Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// NewStoreWithDB reuses an existing database handle (e.g., long-lived server).
func NewStoreWithDB(db *sql.DB) *Store {
	return &Store{db: db}
}

// Close closes the database connection
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying database handle for reuse (e.g., long-lived servers).
func (s *Store) DB() *sql.DB {
	return s.db
}

// latestIndexID returns the ID of the most recently indexed index
func (s *Store) latestIndexID(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM indexes ORDER BY last_indexed DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no indexes found")
	}
	return id, err
}

// EntryResult represents a query result entry
type EntryResult struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	IsDir      bool      `json:"is_dir"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mod_time"`
	Inode      uint64    `json:"inode"`
	TotalSize  int64     `json:"total_size,omitempty"`
	TotalFiles int64     `json:"total_files,omitempty"`
	TotalDirs  int64     `json:"total_dirs,omitempty"`
}

// SearchEntries performs a name search against the latest index.
func (s *Store) SearchEntries(pattern string, limit int) ([]EntryResult, error) {
	ctx := context.Background()
	if limit <= 0 {
		limit = 100
	}

	indexID, err := s.latestIndexID(ctx)
	if err != nil {
		return []EntryResult{}, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT relative_path, name, is_dir, size, mod_time, inode
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
		var isDir int
		if err := rows.Scan(&entry.Path, &entry.Name, &isDir, &entry.Size, &modUnix, &entry.Inode); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		entry.IsDir = isDir == 1
		entry.ModTime = time.Unix(modUnix, 0)
		results = append(results, entry)
	}
	return results, rows.Err()
}

// DirSize aggregates total size under the given path (inclusive).
func (s *Store) DirSize(path string) (int64, error) {
	ctx := context.Background()
	indexID, err := s.latestIndexID(ctx)
	if err != nil {
		return 0, nil
	}

	var total sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT SUM(size)
		FROM entries
		WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?)
	`, indexID, path, path+"%").Scan(&total); err != nil {
		return 0, fmt.Errorf("dir size query failed: %w", err)
	}
	if total.Valid {
		return total.Int64, nil
	}
	return 0, nil
}

// UpsertEntry inserts or replaces a single entry (used for manual updates via API).
// Path should be the relative path (e.g., "/dir/file.txt"), absPath is the full filesystem path.
func (s *Store) UpsertEntry(indexID int64, entry EntryResult, absPath, typ string, hidden bool) error {
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO entries (
			index_id, relative_path, absolute_path, name, size, mod_time, type, hidden, is_dir, inode
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(index_id, relative_path) DO UPDATE SET
			absolute_path=excluded.absolute_path,
			name=excluded.name,
			size=excluded.size,
			mod_time=excluded.mod_time,
			type=excluded.type,
			hidden=excluded.hidden,
			is_dir=excluded.is_dir,
			inode=excluded.inode;
	`, indexID, entry.Path, absPath, entry.Name, entry.Size, entry.ModTime.Unix(), typ, boolToInt(hidden), boolToInt(entry.IsDir), entry.Inode)
	return err
}

// QueryPath queries entries at or under a given path
func (s *Store) QueryPath(path string, recursive bool, limit, offset int) ([]EntryResult, error) {
	ctx := context.Background()
	indexID, err := s.latestIndexID(ctx)
	if err != nil {
		return []EntryResult{}, nil
	}

	var query string
	var args []interface{}

	if recursive {
		query = `
			SELECT relative_path, name, is_dir, size, mod_time, inode
			FROM entries
			WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?)
			ORDER BY relative_path
		`
		args = []interface{}{indexID, path, path + "%"}
	} else {
		query = `
			SELECT relative_path, name, is_dir, size, mod_time, inode
			FROM entries
			WHERE index_id = ? AND relative_path = ?
		`
		args = []interface{}{indexID, path}
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
		var isDir int

		err := rows.Scan(&entry.Path, &entry.Name, &isDir, &entry.Size, &modUnix, &entry.Inode)
		if err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}

		entry.IsDir = isDir == 1
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
	DatabaseSize int64     `json:"database_size"`
}

// GetStats returns database statistics
func (s *Store) GetStats() (*Stats, error) {
	ctx := context.Background()
	var stats Stats

	// Get total indexes
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM indexes`).Scan(&stats.TotalIndexes)
	if err != nil {
		return nil, err
	}

	// Get total entries and size from latest index
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

	// Get database file size
	if fi, err := os.Stat(defaultDBPath); err == nil {
		stats.DatabaseSize = fi.Size()
	}

	return &stats, nil
}
