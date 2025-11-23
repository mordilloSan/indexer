package indexing

import (
	"path/filepath"
	"strings"
	"time"

	"indexer/indexing/iteminfo"
)

// IndexEntry captures a flattened representation of either a directory or a file.
type IndexEntry struct {
	RelativePath string
	AbsolutePath string
	Name         string
	Size         int64
	ModTime      time.Time
	Type         string
	Hidden       bool
	IsDir        bool
	Inode        uint64
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
