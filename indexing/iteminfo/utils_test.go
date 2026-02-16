package iteminfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSymlinks(t *testing.T) {
	// Create temporary directory for testing
	tempDir, err := os.MkdirTemp("", "symlink-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Fatalf("Failed to remove temp dir: %v", err)
		}
	}()

	// Create a target file
	targetFile := filepath.Join(tempDir, "target.txt")
	if err := os.WriteFile(targetFile, []byte("target content"), 0644); err != nil {
		t.Fatalf("Failed to create target file: %v", err)
	}

	// Create a target directory
	targetDir := filepath.Join(tempDir, "targetdir")
	if err := os.Mkdir(targetDir, 0755); err != nil {
		t.Fatalf("Failed to create target directory: %v", err)
	}

	// Create symlink to file
	symlinkFile := filepath.Join(tempDir, "link-to-file")
	if err := os.Symlink(targetFile, symlinkFile); err != nil {
		t.Fatalf("Failed to create symlink to file: %v", err)
	}

	// Create symlink to directory
	symlinkDir := filepath.Join(tempDir, "link-to-dir")
	if err := os.Symlink(targetDir, symlinkDir); err != nil {
		t.Fatalf("Failed to create symlink to directory: %v", err)
	}

	tests := []struct {
		name          string
		path          string
		expectedIsDir bool
		shouldError   bool
	}{
		{
			name:          "regular file",
			path:          targetFile,
			expectedIsDir: false,
			shouldError:   false,
		},
		{
			name:          "regular directory",
			path:          targetDir,
			expectedIsDir: true,
			shouldError:   false,
		},
		{
			name:          "symlink to file",
			path:          symlinkFile,
			expectedIsDir: false,
			shouldError:   false,
		},
		{
			name:          "symlink to directory",
			path:          symlinkDir,
			expectedIsDir: true,
			shouldError:   false,
		},
		{
			name:        "non-existent path",
			path:        filepath.Join(tempDir, "nonexistent"),
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolvedPath, isDir, err := ResolveSymlinks(tt.path)

			if tt.shouldError {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if isDir != tt.expectedIsDir {
				t.Errorf("Expected isDir=%v, got %v", tt.expectedIsDir, isDir)
			}

			// Verify resolved path exists
			if _, err := os.Stat(resolvedPath); err != nil {
				t.Errorf("Resolved path does not exist: %v", err)
			}
		})
	}
}

func TestResolveSymlinks_ChainedLinks(t *testing.T) {
	// Create temporary directory for testing
	tempDir, err := os.MkdirTemp("", "symlink-chain-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Fatalf("Failed to remove temp dir: %v", err)
		}
	}()

	// Create a target file
	targetFile := filepath.Join(tempDir, "target.txt")
	if err := os.WriteFile(targetFile, []byte("target content"), 0644); err != nil {
		t.Fatalf("Failed to create target file: %v", err)
	}

	// Create chain of symlinks: link3 -> link2 -> link1 -> target
	link1 := filepath.Join(tempDir, "link1")
	link2 := filepath.Join(tempDir, "link2")
	link3 := filepath.Join(tempDir, "link3")

	if err := os.Symlink(targetFile, link1); err != nil {
		t.Fatalf("Failed to create link1: %v", err)
	}
	if err := os.Symlink(link1, link2); err != nil {
		t.Fatalf("Failed to create link2: %v", err)
	}
	if err := os.Symlink(link2, link3); err != nil {
		t.Fatalf("Failed to create link3: %v", err)
	}

	// Resolve the chain
	resolvedPath, isDir, err := ResolveSymlinks(link3)
	if err != nil {
		t.Fatalf("Failed to resolve symlink chain: %v", err)
	}

	if isDir {
		t.Error("Expected file, got directory")
	}

	// The resolved path should be the target file
	if resolvedPath != targetFile {
		t.Errorf("Expected resolved path to be %s, got %s", targetFile, resolvedPath)
	}
}

func TestIsDirectory(t *testing.T) {
	// Create temporary directory for testing
	tempDir, err := os.MkdirTemp("", "isdir-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			t.Fatalf("Failed to remove temp dir: %v", err)
		}
	}()

	// Create a file
	testFile := filepath.Join(tempDir, "file.txt")
	if err := os.WriteFile(testFile, []byte("content"), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Create a directory
	testDir := filepath.Join(tempDir, "subdir")
	if err := os.Mkdir(testDir, 0755); err != nil {
		t.Fatalf("Failed to create test directory: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		expected bool
	}{
		{
			name:     "file should not be directory",
			path:     testFile,
			expected: false,
		},
		{
			name:     "directory should be directory",
			path:     testDir,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := os.Stat(tt.path)
			if err != nil {
				t.Fatalf("Failed to stat path: %v", err)
			}

			result := IsDirectory(info)
			if result != tt.expected {
				t.Errorf("Expected IsDirectory=%v, got %v", tt.expected, result)
			}
		})
	}
}

func TestFileInfoSortItems_EmptyLists(t *testing.T) {
	info := &FileInfo{
		Files:   []ItemInfo{},
		Folders: []ItemInfo{},
	}

	// Should not panic with empty lists
	info.SortItems()

	if len(info.Files) != 0 {
		t.Error("Files list should remain empty")
	}
	if len(info.Folders) != 0 {
		t.Error("Folders list should remain empty")
	}
}

func TestFileInfoSortItems_SingleItem(t *testing.T) {
	info := &FileInfo{
		Files: []ItemInfo{
			{Name: "single.txt"},
		},
		Folders: []ItemInfo{
			{Name: "folder"},
		},
	}

	// Should handle single item without error
	info.SortItems()

	if info.Files[0].Name != "single.txt" {
		t.Error("Single file name should remain unchanged")
	}
	if info.Folders[0].Name != "folder" {
		t.Error("Single folder name should remain unchanged")
	}
}
