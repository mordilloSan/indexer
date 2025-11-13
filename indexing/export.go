package indexing

import (
	"path/filepath"
	"sort"
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

// ExportEntries returns a deterministic snapshot of all indexed items.
func (idx *Index) ExportEntries() []IndexEntry {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if len(idx.Directories) == 0 {
		return nil
	}

	keys := make([]string, 0, len(idx.Directories))
	for path := range idx.Directories {
		keys = append(keys, path)
	}
	sort.Strings(keys)

	entries := make([]IndexEntry, 0, len(keys)*2)
	for _, relPath := range keys {
		dirInfo := idx.Directories[relPath]
		absDirPath := idx.realPathFromCombined(relPath)
		normalized := normalizeRelativePath(relPath)

		entries = append(entries, IndexEntry{
			RelativePath: normalized,
			AbsolutePath: absDirPath,
			Name:         directoryName(dirInfo, normalized),
			Size:         dirInfo.Size,
			ModTime:      dirInfo.ModTime,
			Type:         dirInfo.Type,
			Hidden:       dirInfo.Hidden,
			IsDir:        true,
			Inode:        dirInfo.Inode,
		})

		for _, file := range dirInfo.Files {
			entries = append(entries, IndexEntry{
				RelativePath: makeChildRelativePath(normalized, file.Name),
				AbsolutePath: filepath.Join(absDirPath, file.Name),
				Name:         file.Name,
				Size:         file.Size,
				ModTime:      file.ModTime,
				Type:         file.Type,
				Hidden:       file.Hidden,
				IsDir:        false,
				Inode:        file.Inode,
			})
		}
	}

	return entries
}

// IncludeHidden reports whether hidden files were considered during indexing.
func (idx *Index) IncludeHidden() bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.includeHidden
}

func normalizeRelativePath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	clean := "/" + strings.Trim(p, "/")
	return clean
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
