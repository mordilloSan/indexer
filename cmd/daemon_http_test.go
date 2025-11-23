package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"indexer/indexing"
	"indexer/storage"
)

// Exercise the manual add/delete handlers to ensure DB mutations and size propagation work.
func TestHandleAddAndDelete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	db, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	d := &daemon{
		cfg: DaemonConfig{
			IndexName: "test",
			IndexPath: "/",
		},
		db: db,
	}

	// Seed an index row so latestIndexID works.
	if _, err := db.Exec(`
		INSERT INTO indexes (
			name, root_path, source, include_hidden,
			num_dirs, num_files, total_size, disk_used,
			disk_total, last_indexed, index_duration_ms,
			export_duration_ms, vacuum_duration_ms
		) VALUES (?, ?, ?, 0, 0, 0, 0, 0, 0, strftime('%s','now'), 0, 0, 0);
	`, d.cfg.IndexName, d.cfg.IndexPath, d.cfg.IndexPath); err != nil {
		t.Fatalf("insert index: %v", err)
	}
	indexID, err := d.latestIndexID(context.Background())
	if err != nil {
		t.Fatalf("latest index id: %v", err)
	}

	// Seed root and parent directory rows so size propagation has targets.
	seedDir := func(rel string) {
		entry := indexing.IndexEntry{
			RelativePath: rel,
			AbsolutePath: rel,
			Name:         rel,
			Size:         0,
			ModTime:      time.Now(),
			Type:         "directory",
			Hidden:       false,
			IsDir:        true,
			Inode:        0,
		}
		if _, err := storage.UpdateEntry(context.Background(), db, indexID, entry); err != nil {
			t.Fatalf("seed dir %s: %v", rel, err)
		}
	}
	seedDir("/")
	seedDir("/docs")

	// Add a file.
	addPayload := map[string]any{
		"path":   "/docs/file.txt",
		"name":   "file.txt",
		"size":   120,
		"isDir":  false,
		"hidden": false,
	}
	body, _ := json.Marshal(addPayload)
	req := httptest.NewRequest(http.MethodPost, "/add", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	d.handleAdd(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("handleAdd status = %d, body=%s", rr.Code, rr.Body.String())
	}

	assertSize := func(rel string, want int64) {
		var got int64
		if err := db.QueryRow(`SELECT size FROM entries WHERE relative_path = ?`, rel).Scan(&got); err != nil {
			t.Fatalf("query size %s: %v", rel, err)
		}
		if got != want {
			t.Fatalf("size for %s = %d, want %d", rel, got, want)
		}
	}
	assertSize("/docs", 120)
	assertSize("/", 120)

	// Delete the file.
	delReq := httptest.NewRequest(http.MethodDelete, "/delete?path=/docs/file.txt", nil)
	delRec := httptest.NewRecorder()
	d.handleDelete(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("handleDelete status = %d, body=%s", delRec.Code, delRec.Body.String())
	}

	assertSize("/docs", 0)
	assertSize("/", 0)

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entries WHERE relative_path = '/docs/file.txt'`).Scan(&count); err != nil {
		t.Fatalf("count file row: %v", err)
	}
	if count != 0 {
		t.Fatalf("file row still present after delete")
	}
}
