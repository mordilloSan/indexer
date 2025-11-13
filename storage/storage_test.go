package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"indexer/indexing"
	"indexer/indexing/testhelpers"
)

func TestLoadSnapshotIntoIndex(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()
	mock.CreateStandardTestStructure()

	idx := indexing.Initialize("root", mock.Root, mock.Root, true)
	if err := idx.StartIndexing(); err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	tempDB := filepath.Join(t.TempDir(), "snapshot.db")
	metrics := &PersistMetrics{}
	if err := SaveIndexToFile(context.Background(), tempDB, idx, metrics); err != nil {
		t.Fatalf("SaveIndexToFile failed: %v", err)
	}

	fresh := indexing.Initialize("root", mock.Root, mock.Root, true)
	if err := LoadSnapshotIntoIndex(context.Background(), tempDB, "root", fresh); err != nil {
		t.Fatalf("LoadSnapshotIntoIndex failed: %v", err)
	}

	if len(fresh.Directories) == 0 {
		t.Fatalf("expected directories to be populated")
	}
	if _, ok := fresh.Directories["/"]; !ok {
		t.Fatalf("root directory missing after snapshot load")
	}
}

func TestLoadSnapshotIntoIndexMissing(t *testing.T) {
	idx := indexing.Initialize("root", "/does/not/matter", "/does/not/matter", true)
	err := LoadSnapshotIntoIndex(context.Background(), filepath.Join(t.TempDir(), "missing.db"), "root", idx)
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound, got %v", err)
	}
}
