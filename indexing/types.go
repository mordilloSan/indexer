package indexing

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/mordilloSan/indexer/indexing/iteminfo"
)

// IndexEntry captures a flattened representation of either a directory or a file.
type IndexEntry struct {
	RelativePath string
	AbsolutePath string
	Name         string
	Size         int64
	ModTime      time.Time
	Type         string // "directory" or "file"
	Hidden       bool
	Inode        uint64
}

// NormalizeIndexPath returns the canonical relative path representation used throughout the index:
// always leading "/", no trailing "/" (except for root which stays "/").
func NormalizeIndexPath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return "/" + strings.Trim(p, "/")
}

// BoolToInt returns 1 for true, 0 for false.
func BoolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func makeChildRelativePath(parent, child string) string {
	if parent == "/" {
		return filepath.Join("/", child)
	}
	parent = strings.TrimSuffix(parent, "/")
	return filepath.Join(parent, child)
}

func directoryName(info *iteminfo.FileInfo, fallbackPath string) string {
	if info != nil && info.Name != "" {
		return info.Name
	}
	trimmed := strings.Trim(fallbackPath, "/")
	if trimmed == "" {
		return "/"
	}
	parts := strings.Split(trimmed, "/")
	return parts[len(parts)-1]
}
