package indexing

import (
	"os"
	"testing"

	"indexer/indexing/testhelpers"
)

func TestInitialize(t *testing.T) {
	tests := []struct {
		name          string
		indexName     string
		path          string
		source        string
		includeHidden bool
	}{
		{
			name:          "basic initialization",
			indexName:     "test-index",
			path:          "/tmp/test",
			source:        "test-source",
			includeHidden: false,
		},
		{
			name:          "with hidden files",
			indexName:     "test-hidden",
			path:          "/tmp/test2",
			source:        "test-source-2",
			includeHidden: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := Initialize(tt.indexName, tt.path, tt.source, tt.includeHidden)

			if idx.Name != tt.indexName {
				t.Errorf("Expected name %s, got %s", tt.indexName, idx.Name)
			}
			if idx.Path != tt.path {
				t.Errorf("Expected path %s, got %s", tt.path, idx.Path)
			}
			if idx.Source != tt.source {
				t.Errorf("Expected source %s, got %s", tt.source, idx.Source)
			}
			if idx.includeHidden != tt.includeHidden {
				t.Errorf("Expected includeHidden %v, got %v", tt.includeHidden, idx.includeHidden)
			}
			if idx.Status != READY {
				t.Errorf("Expected status READY, got %s", idx.Status)
			}
			if idx.Directories == nil {
				t.Error("Expected Directories map to be initialized")
			}
		})
	}
}

func TestIndexDirectory_BasicStructure(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	mock.CreateStandardTestStructure()

	// Test without hidden files
	idx := Initialize("test", mock.Root, mock.Root, false)
	err := idx.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	// Verify basic structure
	if idx.NumDirs == 0 {
		t.Error("Expected NumDirs > 0")
	}
	if idx.NumFiles == 0 {
		t.Error("Expected NumFiles > 0")
	}

	// Check that root directory was indexed
	rootInfo, exists := idx.Directories["/"]
	if !exists {
		t.Fatal("Root directory not found in index")
	}

	// Verify folders are present
	foundDocuments := false
	foundPhotos := false
	foundCode := false

	for _, folder := range rootInfo.Folders {
		switch folder.Name {
		case "documents":
			foundDocuments = true
		case "photos":
			foundPhotos = true
		case "code":
			foundCode = true
		}
	}

	if !foundDocuments {
		t.Error("documents folder not found")
	}
	if !foundPhotos {
		t.Error("photos folder not found")
	}
	if !foundCode {
		t.Error("code folder not found")
	}
}

func TestIndexDirectory_HiddenFiles(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	mock.CreateStandardTestStructure()

	// Test WITH hidden files
	idxWithHidden := Initialize("test-with-hidden", mock.Root, mock.Root, true)
	err := idxWithHidden.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing with hidden failed: %v", err)
	}

	filesWithHidden := idxWithHidden.NumFiles

	// Test WITHOUT hidden files
	idxWithoutHidden := Initialize("test-without-hidden", mock.Root, mock.Root, false)
	err = idxWithoutHidden.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing without hidden failed: %v", err)
	}

	filesWithoutHidden := idxWithoutHidden.NumFiles

	// With hidden files should have MORE files than without
	if filesWithHidden <= filesWithoutHidden {
		t.Errorf("Expected more files with hidden (%d) than without (%d)", filesWithHidden, filesWithoutHidden)
	}

	// Check that hidden files are actually present when includeHidden=true
	rootInfo, exists := idxWithHidden.Directories["/"]
	if !exists {
		t.Fatal("Root directory not found in index with hidden")
	}

	foundHidden := false
	for _, file := range rootInfo.Files {
		if file.Name == ".config" || file.Name == ".hidden_file" {
			foundHidden = true
			if !file.Hidden {
				t.Errorf("File %s should be marked as hidden", file.Name)
			}
		}
	}

	if !foundHidden {
		t.Error("Expected to find hidden files in root when includeHidden=true")
	}
}

func TestIndexDirectory_NestedStructure(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	mock.CreateStandardTestStructure()

	idx := Initialize("test", mock.Root, mock.Root, false)
	err := idx.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	// Check nested directory: /documents/archive/2023/
	archivePath := "/documents/archive/2023/"
	archiveInfo, exists := idx.Directories[archivePath]
	if !exists {
		t.Fatalf("Nested directory %s not found in index", archivePath)
	}

	// Verify files in the nested directory
	expectedFiles := map[string]bool{
		"jan.txt": false,
		"feb.txt": false,
	}

	for _, file := range archiveInfo.Files {
		if _, ok := expectedFiles[file.Name]; ok {
			expectedFiles[file.Name] = true
		}
	}

	for fileName, found := range expectedFiles {
		if !found {
			t.Errorf("Expected file %s not found in %s", fileName, archivePath)
		}
	}
}

func TestGetTotalSize(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	// Create files with known content sizes
	mock.CreateFile("file1.txt", "12345")      // 5 bytes
	mock.CreateFile("file2.txt", "1234567890") // 10 bytes

	idx := Initialize("test", mock.Root, mock.Root, false)
	err := idx.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	totalSize := idx.GetTotalSize()
	if totalSize == 0 {
		t.Error("Expected totalSize > 0")
	}
}

func TestHardlinks(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	// Create original file
	mock.CreateFile("original.txt", "This is the original content")

	// Create hardlink
	mock.CreateHardlink("original.txt", "hardlink.txt")

	idx := Initialize("test", mock.Root, mock.Root, false)
	err := idx.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	// Should count 2 files
	if idx.NumFiles != 2 {
		t.Errorf("Expected 2 files, got %d", idx.NumFiles)
	}

	// Should track the hardlink
	if len(idx.FoundHardLinks) == 0 {
		t.Error("Expected hardlinks to be tracked")
	}
}

func TestSearch(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	mock.CreateStandardTestStructure()

	idx := Initialize("test", mock.Root, mock.Root, false)
	err := idx.StartIndexing()
	if err != nil {
		t.Fatalf("StartIndexing failed: %v", err)
	}

	tests := []struct {
		name          string
		searchTerm    string
		caseSensitive bool
		expectMin     int // minimum number of results expected
		expectMax     int // maximum number of results expected (0 = no max)
	}{
		{
			name:          "case insensitive search for 'txt'",
			searchTerm:    "txt",
			caseSensitive: false,
			expectMin:     5, // readme.txt, notes.txt, old.txt, jan.txt, feb.txt, numbered/*.txt
		},
		{
			name:          "case sensitive search for 'Report'",
			searchTerm:    "Report",
			caseSensitive: true,
			expectMin:     1,
			expectMax:     1,
		},
		{
			name:          "case insensitive search for 'report'",
			searchTerm:    "report",
			caseSensitive: false,
			expectMin:     1,
		},
		{
			name:          "search for 'image'",
			searchTerm:    "image",
			caseSensitive: false,
			expectMin:     2, // image1.jpg, image2.png
		},
		{
			name:          "search for folder 'documents'",
			searchTerm:    "documents",
			caseSensitive: false,
			expectMin:     1,
		},
		{
			name:          "search for non-existent",
			searchTerm:    "nonexistent123",
			caseSensitive: false,
			expectMin:     0,
			expectMax:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := idx.Search(tt.searchTerm, tt.caseSensitive)

			if len(results) < tt.expectMin {
				t.Errorf("Expected at least %d results, got %d", tt.expectMin, len(results))
			}

			if tt.expectMax > 0 && len(results) > tt.expectMax {
				t.Errorf("Expected at most %d results, got %d", tt.expectMax, len(results))
			}

			// Verify result structure
			for _, result := range results {
				if result.Path == "" {
					t.Error("Result path should not be empty")
				}
				if result.Name == "" {
					t.Error("Result name should not be empty")
				}
			}
		})
	}
}

func TestShouldSkip(t *testing.T) {
	tests := []struct {
		name          string
		isDir         bool
		isHidden      bool
		fullCombined  string
		includeHidden bool
		shouldSkip    bool
	}{
		{
			name:          "root directory never skipped",
			isDir:         true,
			isHidden:      false,
			fullCombined:  "/",
			includeHidden: false,
			shouldSkip:    false,
		},
		{
			name:          "hidden file skipped when includeHidden=false",
			isDir:         false,
			isHidden:      true,
			fullCombined:  "/.hidden",
			includeHidden: false,
			shouldSkip:    true,
		},
		{
			name:          "hidden file not skipped when includeHidden=true",
			isDir:         false,
			isHidden:      true,
			fullCombined:  "/.hidden",
			includeHidden: true,
			shouldSkip:    false,
		},
		{
			name:          "regular file not skipped",
			isDir:         false,
			isHidden:      false,
			fullCombined:  "/file.txt",
			includeHidden: false,
			shouldSkip:    false,
		},
		{
			name:          "regular directory not skipped",
			isDir:         true,
			isHidden:      false,
			fullCombined:  "/documents",
			includeHidden: false,
			shouldSkip:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := Initialize("test", "/tmp/test", "test", tt.includeHidden)
			result := idx.shouldSkip(tt.isDir, tt.isHidden, tt.fullCombined)

			if result != tt.shouldSkip {
				t.Errorf("Expected shouldSkip=%v, got %v", tt.shouldSkip, result)
			}
		})
	}
}

func TestIsHidden(t *testing.T) {
	mock := testhelpers.NewMockFileSystem(t)
	defer mock.Cleanup()

	// Create hidden and regular files
	mock.CreateFile(".hidden", "hidden")
	mock.CreateFile("visible.txt", "visible")

	tests := []struct {
		name       string
		filename   string
		wantHidden bool
	}{
		{"hidden file", ".hidden", true},
		{"visible file", "visible.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := os.Stat(mock.Root + "/" + tt.filename)
			if err != nil {
				t.Fatalf("Failed to stat file: %v", err)
			}

			result := isHidden(info)
			if result != tt.wantHidden {
				t.Errorf("Expected isHidden=%v for %s, got %v", tt.wantHidden, tt.filename, result)
			}
		})
	}
}
