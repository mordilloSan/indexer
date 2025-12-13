package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/mordilloSan/indexer/indexing"
)

func TestGetDirectSubfolders(t *testing.T) {
	ctx, db, dbPath := setupTestDB(t)
	indexID := insertTestIndex(t, db)
	seedDirectories(t, ctx, db, indexID, []directorySeed{
		{path: "/", size: 0},
		{path: "/data", size: 100},
		{path: "/home", size: 200},
		{path: "/home/user", size: 50},
		{path: "/etc", size: 300},
	})

	store := NewStoreWithDB(db, dbPath)

	t.Run("root", func(t *testing.T) {
		assertSubfolders(t, ctx, store, "/", []folderExpectation{
			{name: "data", size: 100},
			{name: "etc", size: 300},
			{name: "home", size: 200},
		})
	})

	t.Run("home", func(t *testing.T) {
		assertSubfolders(t, ctx, store, "/home", []folderExpectation{
			{name: "user", size: 50},
		})
	})
}

func TestSearchEntriesWithType(t *testing.T) {
	ctx, db, dbPath := setupTestDB(t)
	indexID := insertTestIndex(t, db)

	// Create a folder and a file with similar names
	folder := indexing.IndexEntry{
		RelativePath: "/data",
		AbsolutePath: "/data",
		Name:         "data",
		Size:         100,
		ModTime:      time.Now(),
		Type:         "directory",
		Hidden:       false,
		Inode:        1,
	}
	file := indexing.IndexEntry{
		RelativePath: "/data.txt",
		AbsolutePath: "/data.txt",
		Name:         "data.txt",
		Size:         50,
		ModTime:      time.Now(),
		Type:         "file",
		Hidden:       false,
		Inode:        2,
	}

	if _, err := UpdateEntry(ctx, db, indexID, folder); err != nil {
		t.Fatalf("insert folder: %v", err)
	}
	if _, err := UpdateEntry(ctx, db, indexID, file); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	store := NewStoreWithDB(db, dbPath)

	// Search for "data" should return both
	results, err := store.SearchEntries(ctx, "data", 100)
	if err != nil {
		t.Fatalf("search entries: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Verify type field is set correctly
	for _, result := range results {
		if result.Type != "folder" && result.Type != "file" {
			t.Errorf("entry has invalid type=%s, want folder or file", result.Type)
		}
	}
}

type folderExpectation struct {
	name string
	size int64
}

type directorySeed struct {
	path string
	size int64
}

func setupTestDB(t *testing.T) (context.Context, *sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if cerr := db.Close(); cerr != nil {
			t.Fatalf("close db: %v", cerr)
		}
	})

	return ctx, db, dbPath
}

func insertTestIndex(t *testing.T, db *sql.DB) int64 {
	t.Helper()
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
	return indexID
}

func seedDirectories(t *testing.T, ctx context.Context, db *sql.DB, indexID int64, dirs []directorySeed) {
	t.Helper()
	for _, dir := range dirs {
		entry := indexing.IndexEntry{
			RelativePath: dir.path,
			AbsolutePath: dir.path,
			Name:         filepath.Base(dir.path),
			Size:         dir.size,
			ModTime:      time.Now(),
			Type:         "directory",
			Hidden:       false,
			Inode:        0,
		}
		if dir.path == "/" {
			entry.Name = "/"
		}
		if _, err := UpdateEntry(ctx, db, indexID, entry); err != nil {
			t.Fatalf("seed dir %s: %v", dir.path, err)
		}
	}
}

func assertSubfolders(t *testing.T, ctx context.Context, store *Store, path string, expected []folderExpectation) {
	t.Helper()
	results, err := store.GetDirectSubfolders(ctx, path)
	if err != nil {
		t.Fatalf("get direct subfolders (%s): %v", path, err)
	}
	if len(results) != len(expected) {
		t.Fatalf("expected %d subfolders in %s, got %d", len(expected), path, len(results))
	}
	for i, exp := range expected {
		if results[i].Name != exp.name {
			t.Errorf("result[%d] name = %s, want %s", i, results[i].Name, exp.name)
		}
		if results[i].Size != exp.size {
			t.Errorf("result[%d] size = %d, want %d", i, results[i].Size, exp.size)
		}
	}
}
