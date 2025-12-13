package iteminfo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// FileOptions are the options when getting a file info.
type FileOptions struct {
	Username   string // username for access control
	Path       string // realpath
	Source     string
	Expand     bool
	ReadHeader bool
	Content    bool
	Recursive  bool // whether to recursively index directories
	Metadata   bool // whether to get metadata
}

func (info *FileInfo) SortItems() {
	less := naturalSortLess
	sort.Slice(info.Folders, func(i, j int) bool {
		return less(info.Folders[i].Name, info.Folders[j].Name)
	})
	sort.Slice(info.Files, func(i, j int) bool {
		return less(info.Files[i].Name, info.Files[j].Name)
	})
}

// naturalSortLess compares two names with natural (numeric-aware) sorting
func naturalSortLess(a, b string) bool {
	baseA := strings.Split(a, ".")[0]
	baseB := strings.Split(b, ".")[0]

	numA, errA := strconv.Atoi(baseA)
	numB, errB := strconv.Atoi(baseB)

	if errA == nil && errB == nil {
		return numA < numB
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

// ResolveSymlinks resolves symlinks in the given path and returns
// the final resolved path, whether it's a directory (considering bundle logic), and any error.
func ResolveSymlinks(path string) (string, bool, error) {
	for {
		// Get the file info using os.Lstat to handle symlinks
		info, err := os.Lstat(path)
		if err != nil {
			return path, false, fmt.Errorf("could not stat path: %s, %v", path, err)
		}

		// Check if the path is a symlink
		if info.Mode()&os.ModeSymlink != 0 {
			// Read the symlink target
			target, err := os.Readlink(path)
			if err != nil {
				return path, false, fmt.Errorf("could not read symlink: %s, %v", path, err)
			}

			// If target is absolute, use it directly; otherwise resolve relative to symlink's directory
			if filepath.IsAbs(target) {
				path = target
			} else {
				path = filepath.Join(filepath.Dir(path), target)
			}
		} else {
			// Not a symlink, check with bundle-aware directory logic
			isDir := IsDirectory(info)
			return path, isDir, nil
		}
	}
}

func GetParentDirectoryPath(path string) string {
	if path == "/" || path == "" {
		return ""
	}
	path = strings.TrimSuffix(path, "/")
	lastSlash := strings.LastIndex(path, "/")

	switch lastSlash {
	case -1:
		return "" // No parent for relative path without slashes
	case 0:
		return "/" // Parent of /foo is /
	default:
		return path[:lastSlash]
	}
}

// ContainsSearchTerm checks if the item name contains the search term
func (fi ItemInfo) ContainsSearchTerm(searchTerm string, options SearchOptions) bool {
	fileName := fi.Name
	if !options.CaseSensitive {
		fileName = strings.ToLower(fileName)
		searchTerm = strings.ToLower(searchTerm)
	}
	return strings.Contains(fileName, searchTerm)
}

// IsDirectory determines if a path should be treated as a directory
func IsDirectory(fileInfo os.FileInfo) bool {
	return fileInfo.IsDir()
}
