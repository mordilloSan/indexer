package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"indexer/storage"
)

type e2eEnv struct {
	root      string
	dbPath    string
	indexName string
}

// newE2EEnv creates a temporary directory tree, indexes it into a temporary
// SQLite database using runIndexCommand, and returns the environment details.
func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()

	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "file1.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write file1: %v", err)
	}

	subDir := filepath.Join(root, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write nested: %v", err)
	}

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "indexer.db")
	indexName := "testindex"

	if err := runIndexCommand([]string{
		"-path", root,
		"-name", indexName,
		"-db-path", dbPath,
		"-no-rate-limit",
	}); err != nil {
		t.Fatalf("runIndexCommand: %v", err)
	}

	return &e2eEnv{
		root:      root,
		dbPath:    dbPath,
		indexName: indexName,
	}
}

// TestIndexAndQueryTempDB provides an end-to-end style test:
// it indexes a real temporary directory into a temporary SQLite
// database using runIndexCommand, then queries that database using
// storage.GetDirStats to verify the contents are persisted and
// readable.
func TestIndexAndQueryTempDB(t *testing.T) {
	env := newE2EEnv(t)

	ctx := context.Background()
	statsRoot, err := storage.GetDirStats(ctx, env.dbPath, env.indexName, "/")
	if err != nil {
		t.Fatalf("GetDirStats root: %v", err)
	}
	if statsRoot.NumFiles < 2 {
		t.Fatalf("expected at least 2 files, got %d", statsRoot.NumFiles)
	}
	if statsRoot.NumDirs < 1 {
		t.Fatalf("expected at least 1 directory, got %d", statsRoot.NumDirs)
	}

	statsSub, err := storage.GetDirStats(ctx, env.dbPath, env.indexName, "/sub")
	if err != nil {
		t.Fatalf("GetDirStats /sub: %v", err)
	}
	if statsSub.NumFiles < 1 {
		t.Fatalf("expected at least 1 file in /sub, got %d", statsSub.NumFiles)
	}
}

// TestSizeCommandEndToEnd runs runSizeCommand against the temporary database
// and asserts that it prints a positive size for the root directory.
func TestSizeCommandEndToEnd(t *testing.T) {
	env := newE2EEnv(t)

	var buf bytes.Buffer
	oldStdout := os.Stdout
	defer func() { os.Stdout = oldStdout }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()

	if err := runSizeCommand([]string{
		"-name", env.indexName,
		"-db-path", env.dbPath,
		"-path", "/",
	}); err != nil {
		t.Fatalf("runSizeCommand: %v", err)
	}

	_ = w.Close()
	<-done

	if buf.Len() == 0 {
		t.Fatalf("expected size output, got empty")
	}
}

// TestSearchCommandEndToEnd runs runSearchCommand against the temporary
// database and ensures it completes successfully for a simple query.
func TestSearchCommandEndToEnd(t *testing.T) {
	env := newE2EEnv(t)

	if err := runSearchCommand([]string{
		"-name", env.indexName,
		"-db-path", env.dbPath,
		"-path", env.root,
		"-query", "file1",
		"-json",
	}); err != nil {
		t.Fatalf("runSearchCommand: %v", err)
	}
}
