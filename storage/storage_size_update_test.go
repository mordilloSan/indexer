package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"indexer/indexing"
)

// Integration-style test to ensure manual add/delete operations keep ancestor sizes in sync.
func TestUpsertAndDeletePropagatesSizes(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "index.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close db: %v", cerr)
		}
	}()

	// Seed an index row.
	res, err := db.Exec(`
		INSERT INTO indexes (
			name, root_path, source, include_hidden,
			num_dirs, num_files, total_size, disk_used,
			disk_total, last_indexed, index_duration_ms,
			export_duration_ms, vacuum_duration_ms
		) VALUES (?, ?, ?, 0, 0, 0, 0, 0, 0, strftime('%s','now'), 0, 0, 0);
	`, "test", "/", "/")
	if err != nil {
		t.Fatalf("insert index: %v", err)
	}
	indexID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("index id: %v", err)
	}

	// Seed root and a child directory so parent size updates have somewhere to land.
	seedDir := func(relPath string) {
		entry := indexing.IndexEntry{
			RelativePath: relPath,
			AbsolutePath: relPath,
			Name:         filepath.Base(relPath),
			Size:         0,
			ModTime:      time.Now(),
			Type:         "directory",
			Hidden:       false,
			IsDir:        true,
			Inode:        0,
		}
		if relPath == "/" {
			entry.Name = "/"
		}
		if _, err := UpdateEntry(ctx, db, indexID, entry); err != nil {
			t.Fatalf("seed dir %s: %v", relPath, err)
		}
	}
	seedDir("/")
	seedDir("/data")

	getSize := func(relPath string) int64 {
		var size int64
		err := db.QueryRowContext(ctx, `SELECT size FROM entries WHERE index_id = ? AND relative_path = ?`, indexID, relPath).Scan(&size)
		if err != nil {
			t.Fatalf("query size %s: %v", relPath, err)
		}
		return size
	}

	fileEntry := indexing.IndexEntry{
		RelativePath: "/data/file.txt",
		AbsolutePath: "/data/file.txt",
		Name:         "file.txt",
		Size:         200,
		ModTime:      time.Now(),
		Type:         "file",
		Hidden:       false,
		IsDir:        false,
		Inode:        1,
	}

	if err := UpsertEntryWithSizeUpdate(ctx, db, indexID, fileEntry); err != nil {
		t.Fatalf("upsert file: %v", err)
	}

	if got := getSize("/data"); got != 200 {
		t.Fatalf("dir size after add = %d, want 200", got)
	}
	if got := getSize("/"); got != 200 {
		t.Fatalf("root size after add = %d, want 200", got)
	}

	// Increase file size and ensure deltas propagate.
	fileEntry.Size = 350
	if err := UpsertEntryWithSizeUpdate(ctx, db, indexID, fileEntry); err != nil {
		t.Fatalf("upsert file (resize): %v", err)
	}
	if got := getSize("/data"); got != 350 {
		t.Fatalf("dir size after resize = %d, want 350", got)
	}
	if got := getSize("/"); got != 350 {
		t.Fatalf("root size after resize = %d, want 350", got)
	}

	// Delete and ensure sizes drop.
	if err := DeleteEntryWithSizeUpdate(ctx, db, indexID, "/data/file.txt"); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if got := getSize("/data"); got != 0 {
		t.Fatalf("dir size after delete = %d, want 0", got)
	}
	if got := getSize("/"); got != 0 {
		t.Fatalf("root size after delete = %d, want 0", got)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries WHERE index_id = ? AND relative_path = ?`, indexID, "/data/file.txt").Scan(&count); err != nil {
		t.Fatalf("count file row: %v", err)
	}
	if count != 0 {
		t.Fatalf("file row still present after delete (count=%d)", count)
	}
}
