package testhelpers

import (
	"os"
	"path/filepath"
	"testing"
)

// MockFileSystem creates a temporary directory structure for testing
type MockFileSystem struct {
	Root string
	t    *testing.T
}

// NewMockFileSystem creates a new mock filesystem in a temp directory
func NewMockFileSystem(t *testing.T) *MockFileSystem {
	tempDir, err := os.MkdirTemp("", "indexer-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}

	return &MockFileSystem{
		Root: tempDir,
		t:    t,
	}
}

// Cleanup removes the temporary directory
func (m *MockFileSystem) Cleanup() {
	if err := os.RemoveAll(m.Root); err != nil {
		m.t.Errorf("Failed to cleanup temp dir: %v", err)
	}
}

// CreateDir creates a directory in the mock filesystem
func (m *MockFileSystem) CreateDir(path string) {
	fullPath := filepath.Join(m.Root, path)
	if err := os.MkdirAll(fullPath, 0755); err != nil {
		m.t.Fatalf("Failed to create directory %s: %v", path, err)
	}
}

// CreateFile creates a file with the given content
func (m *MockFileSystem) CreateFile(path string, content string) {
	fullPath := filepath.Join(m.Root, path)

	// Ensure parent directory exists
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		m.t.Fatalf("Failed to create parent dir for %s: %v", path, err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		m.t.Fatalf("Failed to create file %s: %v", path, err)
	}
}

// CreateHiddenFile creates a hidden file (starting with .)
func (m *MockFileSystem) CreateHiddenFile(path string, content string) {
	m.CreateFile(path, content)
}

// CreateSymlink creates a symbolic link
func (m *MockFileSystem) CreateSymlink(target, linkPath string) {
	fullTarget := filepath.Join(m.Root, target)
	fullLink := filepath.Join(m.Root, linkPath)

	// Ensure parent directory exists
	parentDir := filepath.Dir(fullLink)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		m.t.Fatalf("Failed to create parent dir for symlink %s: %v", linkPath, err)
	}

	if err := os.Symlink(fullTarget, fullLink); err != nil {
		m.t.Fatalf("Failed to create symlink %s -> %s: %v", linkPath, target, err)
	}
}

// CreateHardlink creates a hard link
func (m *MockFileSystem) CreateHardlink(target, linkPath string) {
	fullTarget := filepath.Join(m.Root, target)
	fullLink := filepath.Join(m.Root, linkPath)

	// Ensure parent directory exists
	parentDir := filepath.Dir(fullLink)
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		m.t.Fatalf("Failed to create parent dir for hardlink %s: %v", linkPath, err)
	}

	if err := os.Link(fullTarget, fullLink); err != nil {
		m.t.Fatalf("Failed to create hardlink %s -> %s: %v", linkPath, target, err)
	}
}

// CreateStandardTestStructure creates a standard test directory structure
func (m *MockFileSystem) CreateStandardTestStructure() {
	// Regular directories and files
	m.CreateDir("documents")
	m.CreateFile("documents/readme.txt", "This is a readme file")
	m.CreateFile("documents/notes.txt", "These are notes")
	m.CreateFile("documents/Report.pdf", "PDF content")

	m.CreateDir("photos")
	m.CreateFile("photos/image1.jpg", "JPEG data")
	m.CreateFile("photos/image2.png", "PNG data")

	m.CreateDir("code")
	m.CreateFile("code/main.go", "package main\n")
	m.CreateFile("code/utils.go", "package main\n")

	// Nested directories
	m.CreateDir("documents/archive")
	m.CreateFile("documents/archive/old.txt", "Old document")

	m.CreateDir("documents/archive/2023")
	m.CreateFile("documents/archive/2023/jan.txt", "January data")
	m.CreateFile("documents/archive/2023/feb.txt", "February data")

	// Hidden files
	m.CreateHiddenFile(".config", "config data")
	m.CreateHiddenFile(".hidden_file", "secret content")
	m.CreateHiddenFile("documents/.git", "git data")

	// Files with numbers (for sorting tests)
	m.CreateDir("numbered")
	m.CreateFile("numbered/1.txt", "one")
	m.CreateFile("numbered/2.txt", "two")
	m.CreateFile("numbered/10.txt", "ten")
	m.CreateFile("numbered/20.txt", "twenty")
	m.CreateFile("numbered/100.txt", "hundred")
}

// GetFileSize returns the size of a file in the mock filesystem
func (m *MockFileSystem) GetFileSize(path string) int64 {
	fullPath := filepath.Join(m.Root, path)
	info, err := os.Stat(fullPath)
	if err != nil {
		m.t.Fatalf("Failed to stat file %s: %v", path, err)
	}
	return info.Size()
}
