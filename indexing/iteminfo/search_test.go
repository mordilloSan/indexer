package iteminfo

import (
	"testing"
	"time"
)

func TestParseSearch(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		caseSensitive bool
		expectedTerms []string
	}{
		{
			name:          "simple search",
			input:         "test",
			caseSensitive: false,
			expectedTerms: []string{"test"},
		},
		{
			name:          "case sensitive search",
			input:         "case:exact Test",
			caseSensitive: true,
			expectedTerms: []string{"Test"},
		},
		{
			name:          "quoted exact phrase",
			input:         "\"exact phrase\"",
			caseSensitive: false,
			expectedTerms: []string{"exact phrase"},
		},
		{
			name:          "multiple terms with OR",
			input:         "term1|term2|term3",
			caseSensitive: false,
			expectedTerms: []string{"term1", "term2", "term3"},
		},
		{
			name:          "case sensitive with OR",
			input:         "case:exact Term1|Term2",
			caseSensitive: true,
			expectedTerms: []string{"Term1", "Term2"},
		},
		{
			name:          "empty string",
			input:         "",
			caseSensitive: false,
			expectedTerms: []string{},
		},
		{
			name:          "whitespace only",
			input:         "   ",
			caseSensitive: false,
			expectedTerms: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseSearch(tt.input)

			if result.CaseSensitive != tt.caseSensitive {
				t.Errorf("Expected CaseSensitive=%v, got %v", tt.caseSensitive, result.CaseSensitive)
			}

			if len(result.Terms) != len(tt.expectedTerms) {
				t.Errorf("Expected %d terms, got %d", len(tt.expectedTerms), len(result.Terms))
				return
			}

			for i, term := range tt.expectedTerms {
				if result.Terms[i] != term {
					t.Errorf("Expected term[%d]=%s, got %s", i, term, result.Terms[i])
				}
			}
		})
	}
}

func TestContainsSearchTerm(t *testing.T) {
	tests := []struct {
		name          string
		fileName      string
		searchTerm    string
		caseSensitive bool
		shouldMatch   bool
	}{
		{
			name:          "exact match case insensitive",
			fileName:      "readme.txt",
			searchTerm:    "readme",
			caseSensitive: false,
			shouldMatch:   true,
		},
		{
			name:          "partial match case insensitive",
			fileName:      "readme.txt",
			searchTerm:    "read",
			caseSensitive: false,
			shouldMatch:   true,
		},
		{
			name:          "case mismatch case insensitive",
			fileName:      "README.TXT",
			searchTerm:    "readme",
			caseSensitive: false,
			shouldMatch:   true,
		},
		{
			name:          "case mismatch case sensitive no match",
			fileName:      "README.TXT",
			searchTerm:    "readme",
			caseSensitive: true,
			shouldMatch:   false,
		},
		{
			name:          "case match case sensitive",
			fileName:      "README.TXT",
			searchTerm:    "README",
			caseSensitive: true,
			shouldMatch:   true,
		},
		{
			name:          "no match",
			fileName:      "readme.txt",
			searchTerm:    "notfound",
			caseSensitive: false,
			shouldMatch:   false,
		},
		{
			name:          "extension match",
			fileName:      "document.pdf",
			searchTerm:    ".pdf",
			caseSensitive: false,
			shouldMatch:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := ItemInfo{
				Name:    tt.fileName,
				Size:    100,
				ModTime: time.Now(),
				Type:    "file",
				Hidden:  false,
			}

			opts := SearchOptions{
				CaseSensitive: tt.caseSensitive,
				Terms:         []string{tt.searchTerm},
			}

			result := item.ContainsSearchTerm(tt.searchTerm, opts)

			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v for '%s' contains '%s' (caseSensitive=%v), got %v",
					tt.shouldMatch, tt.fileName, tt.searchTerm, tt.caseSensitive, result)
			}
		})
	}
}

func TestSortItems(t *testing.T) {
	t.Run("numeric sorting", func(t *testing.T) {
		info := &FileInfo{
			Files: []ItemInfo{
				{Name: "100.txt"},
				{Name: "2.txt"},
				{Name: "10.txt"},
				{Name: "1.txt"},
				{Name: "20.txt"},
			},
		}

		info.SortItems()

		expected := []string{"1.txt", "2.txt", "10.txt", "20.txt", "100.txt"}
		for i, expectedName := range expected {
			if info.Files[i].Name != expectedName {
				t.Errorf("Expected Files[%d]=%s, got %s", i, expectedName, info.Files[i].Name)
			}
		}
	})

	t.Run("alphabetic sorting", func(t *testing.T) {
		info := &FileInfo{
			Folders: []ItemInfo{
				{Name: "zebra"},
				{Name: "apple"},
				{Name: "banana"},
				{Name: "Cherry"},
			},
		}

		info.SortItems()

		expected := []string{"apple", "banana", "Cherry", "zebra"}
		for i, expectedName := range expected {
			if info.Folders[i].Name != expectedName {
				t.Errorf("Expected Folders[%d]=%s, got %s", i, expectedName, info.Folders[i].Name)
			}
		}
	})

	t.Run("mixed numeric and text", func(t *testing.T) {
		info := &FileInfo{
			Files: []ItemInfo{
				{Name: "file100.txt"},
				{Name: "file2.txt"},
				{Name: "file10.txt"},
				{Name: "document.txt"},
			},
		}

		info.SortItems()

		// Numeric filenames should sort numerically, text should sort alphabetically
		// "document.txt" should come first (alphabetically)
		// Then "file2.txt", "file10.txt", "file100.txt" (numerically by prefix)
		if info.Files[0].Name != "document.txt" {
			t.Errorf("Expected first file to be 'document.txt', got %s", info.Files[0].Name)
		}
	})
}

func TestGetParentDirectoryPath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "root has no parent",
			path:     "/",
			expected: "",
		},
		{
			name:     "empty path",
			path:     "",
			expected: "",
		},
		{
			name:     "top level directory",
			path:     "/home",
			expected: "/",
		},
		{
			name:     "nested directory",
			path:     "/home/user/documents",
			expected: "/home/user",
		},
		{
			name:     "path with trailing slash",
			path:     "/home/user/documents/",
			expected: "/home/user",
		},
		{
			name:     "deeply nested",
			path:     "/a/b/c/d/e/f",
			expected: "/a/b/c/d/e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetParentDirectoryPath(tt.path)
			if result != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, result)
			}
		})
	}
}
