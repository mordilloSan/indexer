package indexing

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/indexing/iteminfo"
)

// dirMetadataKey normalizes directory paths to the format used as keys in dirMetadata.
func dirMetadataKey(path string) string {
	return NormalizeIndexPath(path)
}

// indexDirectory recursively indexes files and directories.
func (idx *Index) indexDirectory(adjustedPath string) error {
	// Normalize path to always have trailing slash (except for root which is just "/")
	if adjustedPath != "/" {
		adjustedPath = strings.TrimSuffix(adjustedPath, "/") + "/"
	}
	realPath := strings.TrimRight(idx.Path, "/") + adjustedPath
	// Open the directory
	dir, err := os.Open(realPath)
	if err != nil {
		// must have been deleted
		return err
	}
	defer func() { _ = dir.Close() }()

	dirInfo, err := dir.Stat()
	if err != nil {
		return err
	}

	// check if excluded from indexing
	hidden := isHidden(dirInfo)
	if idx.shouldSkip(dirInfo.IsDir(), hidden, adjustedPath) {
		return ErrNotIndexed
	}

	dirFileInfo, err2 := idx.GetDirInfo(dir, dirInfo, realPath, adjustedPath)
	if err2 != nil {
		return err2
	}

	idx.mu.Lock()
	dirKey := dirMetadataKey(adjustedPath)
	idx.dirMetadata[dirKey] = DirMetadata{
		Size:    dirFileInfo.Size,
		ModTime: dirFileInfo.ModTime,
		Inode:   dirFileInfo.Inode,
	}
	idx.mu.Unlock()

	normalized := dirKey
	absDirPath := idx.realPathFromCombined(adjustedPath)

	entry := IndexEntry{
		RelativePath: normalized,
		AbsolutePath: absDirPath,
		Name:         directoryName(dirFileInfo, normalized),
		Size:         dirFileInfo.Size,
		ModTime:      dirFileInfo.ModTime,
		Type:         "directory",
		Hidden:       dirFileInfo.Hidden,
		Inode:        dirFileInfo.Inode,
	}
	if err := idx.streamWriter.Write(entry); err != nil {
		logger.Errorf("Failed to stream directory entry: %v", err)
	}
	return nil
}

func (idx *Index) GetDirInfo(dirInfo *os.File, stat os.FileInfo, realPath, adjustedPath string) (*iteminfo.FileInfo, error) {
	// Ensure combinedPath has exactly one trailing slash to prevent double slashes in subdirectory paths
	combinedPath := strings.TrimRight(adjustedPath, "/") + "/"
	// Read directory contents
	files, err := dirInfo.Readdir(-1)
	if err != nil {
		return nil, err
	}

	var totalSize int64
	dirHidden := isHidden(stat)
	dirInode := inodeFromFileInfo(stat)
	normalizedDir := dirMetadataKey(adjustedPath)

	for _, file := range files {
		hidden := isHidden(file)
		isDir := iteminfo.IsDirectory(file)
		baseName := file.Name()
		fullCombined := combinedPath + baseName
		if idx.shouldSkip(isDir, hidden, fullCombined) {
			continue
		}

		if isDir {
			dirPath := combinedPath + baseName
			dirMapKey := dirMetadataKey(dirPath)

			if err := idx.indexDirectory(dirPath); err != nil {
				logger.Debugf("Failed to %v", err)
				continue
			}

			idx.mu.Lock()
			if meta, exists := idx.dirMetadata[dirMapKey]; exists {
				totalSize += meta.Size
				delete(idx.dirMetadata, dirMapKey)
			}
			idx.mu.Unlock()

			idx.incrementDirCount()
		} else {
			size, shouldCountSize := idx.handleFile(file, fullCombined)
			if shouldCountSize {
				totalSize += int64(size)
			}
			idx.incrementFileCount()

			childPath := makeChildRelativePath(normalizedDir, baseName)
			absPath := filepath.Join(realPath, baseName)

			entry := IndexEntry{
				RelativePath: childPath,
				AbsolutePath: absPath,
				Name:         baseName,
				Size:         int64(size),
				ModTime:      file.ModTime(),
				Type:         "file",
				Hidden:       hidden,
				Inode:        inodeFromFileInfo(file),
			}
			if err := idx.streamWriter.Write(entry); err != nil {
				logger.Errorf("Failed to stream file entry: %v", err)
			}
		}
	}

	if adjustedPath == "/" {
		idx.mu.Lock()
		idx.DiskUsed = uint64(totalSize)
		idx.mu.Unlock()
	}

	dirFileInfo := &iteminfo.FileInfo{
		Path: adjustedPath,
	}
	dirFileInfo.ItemInfo = iteminfo.ItemInfo{
		Name:    filepath.Base(stat.Name()),
		Type:    "directory",
		Size:    totalSize,
		ModTime: stat.ModTime(),
		Inode:   dirInode,
		Hidden:  dirHidden,
	}
	return dirFileInfo, nil
}

func isHidden(file os.FileInfo) bool {
	// Check if the file starts with a dot (Linux hidden files)
	name := file.Name()
	return len(name) > 0 && name[0] == '.'
}

func (idx *Index) shouldSkip(isDir bool, isHidden bool, fullCombined string) bool {
	if fullCombined == "/" {
		return false
	}

	if isDir {
		// Skip system directories that should never be indexed
		if runtime.GOOS == "linux" {
			if idx.isLinuxSystemPath(fullCombined) {
				return true
			}
			if idx.isExternalMount(fullCombined) {
				return true
			}
		}
	}

	// Skip hidden files and directories unless includeHidden is true
	if isHidden && !idx.includeHidden {
		return true
	}

	return false
}

func (idx *Index) realPathFromCombined(fullCombined string) string {
	base := filepath.Clean(idx.Path)
	if base == "" {
		base = "/"
	}

	cleanCombined := filepath.Clean(fullCombined)
	if cleanCombined == "/" || cleanCombined == "." {
		return base
	}

	relative := strings.TrimPrefix(cleanCombined, "/")
	return filepath.Join(base, relative)
}
