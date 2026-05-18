package cmd

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestOperationLockExcludesConcurrentAcquire(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")

	lock, err := tryAcquireOperationLock(dbPath)
	if err != nil {
		t.Fatalf("tryAcquireOperationLock first: %v", err)
	}

	if _, err := tryAcquireOperationLock(dbPath); !errors.Is(err, errOperationAlreadyRunning) {
		t.Fatalf("second tryAcquireOperationLock err = %v, want errOperationAlreadyRunning", err)
	}

	if err := lock.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}

	lock, err = tryAcquireOperationLock(dbPath)
	if err != nil {
		t.Fatalf("tryAcquireOperationLock after close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("close second lock: %v", err)
	}
}
