package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func TestPruneOldIndexesCountOnly(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()

	for i := 1; i <= 4; i++ {
		result, err := db.ExecContext(ctx, `
			INSERT INTO indexes (
				name, root_path, source, include_hidden,
				num_dirs, num_files, total_size, disk_used,
				disk_total, last_indexed
			) VALUES (?, ?, ?, 0, 0, 1, 1, 1, 1, ?);
		`, fmt.Sprintf("idx%d", i), "/tmp", "/tmp", int64(i))
		if err != nil {
			t.Fatalf("insert index %d: %v", i, err)
		}
		indexID, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("last insert id: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO entries (
				index_id, relative_path, path_depth, name, size,
				mod_time, type, hidden, inode, last_seen
			) VALUES (?, '/', 0, '/', 1, ?, 'directory', 0, 0, ?);
		`, indexID, int64(i), int64(i)); err != nil {
			t.Fatalf("insert entry %d: %v", i, err)
		}
	}

	stats, err := PruneOldIndexes(ctx, db, 2, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.DeletedIndexes != 2 {
		t.Fatalf("deleted indexes = %d, want 2", stats.DeletedIndexes)
	}

	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM indexes;`).Scan(&remaining); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining indexes = %d, want 2", remaining)
	}

	var oldestRemaining int64
	if err := db.QueryRowContext(ctx, `SELECT MIN(last_indexed) FROM indexes;`).Scan(&oldestRemaining); err != nil {
		t.Fatalf("min last_indexed: %v", err)
	}
	if oldestRemaining != 3 {
		t.Fatalf("oldest remaining last_indexed = %d, want 3", oldestRemaining)
	}
}
