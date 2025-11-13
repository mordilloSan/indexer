package iteminfo

import (
	"os"
	"strings"
)

// ContainsSearchTerm checks if the file/folder name contains the search term
func (fi ItemInfo) ContainsSearchTerm(searchTerm string, options SearchOptions) bool {
	fileName := fi.Name

	// Convert to lowercase if not case-sensitive
	if !options.CaseSensitive {
		fileName = strings.ToLower(fileName)
		searchTerm = strings.ToLower(searchTerm)
	}

	// Check if the file name contains the search term
	return strings.Contains(fileName, searchTerm)
}

// IsDirectory determines if a path should be treated as a directory
func IsDirectory(fileInfo os.FileInfo) bool {
	return fileInfo.IsDir()
}
