package indexing

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"indexer/indexing/iteminfo"
)

var (
	// ErrNotIndexed is returned when a path is not indexed
	ErrNotIndexed = errors.New("path not indexed")
)

// reduced index is json exposed to the client
type ReducedIndex struct {
	IdxName         string      `json:"name"`
	DiskUsed        uint64      `json:"used"`
	DiskTotal       uint64      `json:"total"`
	Status          IndexStatus `json:"status"`
	NumDirs         uint64      `json:"numDirs"`
	NumFiles        uint64      `json:"numFiles"`
	LastIndexed     time.Time   `json:"-"`
	LastIndexedUnix int64       `json:"lastIndexedUnixTime"`
}

// DirMetadata stores minimal per-directory metadata; the map key is the path to avoid duplicating it.
type DirMetadata struct {
	Size    int64
	ModTime time.Time
	Inode   uint64
}

// StreamingWriter is an interface for writing entries as they are discovered.
type StreamingWriter interface {
	Write(entry IndexEntry) error
}

type Index struct {
	ReducedIndex
	Name            string              // unique name for this index
	Path            string              // filesystem path being indexed
	Source          string              // source identifier
	includeHidden   bool                // whether to include hidden files and directories
	FoundHardLinks  map[string]uint64   `json:"-"` // hardlink path -> size
	processedInodes map[uint64]struct{} `json:"-"` // tracks processed inodes for hardlinks
	totalSize       uint64              `json:"-"` // total size
	mu              sync.RWMutex        `json:"-"` // protects concurrent access

	// Streaming mode fields
	streamWriter StreamingWriter        `json:"-"` // where to send entries in streaming mode
	dirMetadata  map[string]DirMetadata `json:"-"` // lightweight dir metadata in streaming mode
}

type IndexStatus string

const (
	READY       IndexStatus = "ready"
	INDEXING    IndexStatus = "indexing"
	UNAVAILABLE IndexStatus = "unavailable"
)

var (
	linuxSystemPaths = []string{"/proc", "/dev"}
	externalFSTypes  = map[string]struct{}{
		"nfs":        {},
		"nfs4":       {},
		"cifs":       {},
		"smbfs":      {},
		"smb2":       {},
		"smb3":       {},
		"fuse.cifs":  {},
		"fuse.smb":   {},
		"fuse.smb3":  {},
		"fuse.nfs":   {},
		"fuse.ceph":  {},
		"fuse.iscsi": {},
	}
	externalMountsOnce  sync.Once
	externalMountPoints map[string]string
)

// Initialize creates a new index for the given path
// name: a unique name for this index
// path: the filesystem path to index (e.g., "/", "/home", "/home/user/documents")
// source: an optional source identifier (can be same as name)
// includeHidden: whether to include hidden files and directories (starting with .)
func Initialize(name string, path string, source string, includeHidden bool) *Index {
	newIndex := &Index{
		Name:            name,
		Path:            path,
		Source:          source,
		includeHidden:   includeHidden,
		processedInodes: make(map[uint64]struct{}),
		FoundHardLinks:  make(map[string]uint64),
	}
	newIndex.ReducedIndex = ReducedIndex{
		Status:  READY,
		IdxName: name,
	}

	logger.Infof("initialized index [%s] for path [%s] (includeHidden: %v)", name, path, includeHidden)
	return newIndex
}

// EnableStreaming configures the index to use streaming mode with the provided writer.
// In streaming mode, files are written immediately to the database, and only lightweight
// directory metadata is kept in memory.
func (idx *Index) EnableStreaming(writer StreamingWriter) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.streamWriter = writer
	idx.dirMetadata = make(map[string]DirMetadata)
}

// StartIndexing begins indexing the configured path
func (idx *Index) StartIndexing() error {
	idx.mu.RLock()
	writer := idx.streamWriter
	idx.mu.RUnlock()

	if writer == nil {
		return fmt.Errorf("streaming mode is required; call EnableStreaming with a writer before indexing")
	}

	idx.SetStatus(INDEXING)
	logger.Infof("starting indexing for [%s] at path [%s]", idx.Name, idx.Path)

	err := idx.indexDirectory("/", true)
	if err != nil {
		idx.SetStatus(UNAVAILABLE)
		return err
	}

	idx.SetStatus(READY)
	logger.Infof("completed indexing for [%s]: %d directories, %d files", idx.Name, idx.NumDirs, idx.NumFiles)
	return nil
}

// indexDirectory recursively indexes files and directories
func (idx *Index) indexDirectory(adjustedPath string, recursive bool) error {
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

	// adjustedPath is already normalized with trailing slash
	combinedPath := adjustedPath
	dirFileInfo, err2 := idx.GetDirInfo(dir, dirInfo, realPath, adjustedPath, combinedPath, recursive)
	if err2 != nil {
		return err2
	}

	idx.mu.Lock()
	idx.dirMetadata[adjustedPath] = DirMetadata{
		Size:    dirFileInfo.Size,
		ModTime: dirFileInfo.ModTime,
		Inode:   dirFileInfo.Inode,
	}
	idx.mu.Unlock()

	if recursive {
		normalized := NormalizeIndexPath(adjustedPath)
		absDirPath := idx.realPathFromCombined(adjustedPath)

		entry := IndexEntry{
			RelativePath: normalized,
			AbsolutePath: absDirPath,
			Name:         directoryName(dirFileInfo, normalized),
			Size:         dirFileInfo.Size,
			ModTime:      dirFileInfo.ModTime,
			Type:         "directory",
			Hidden:       dirFileInfo.Hidden,
			IsDir:        true,
			Inode:        dirFileInfo.Inode,
		}
		if err := idx.streamWriter.Write(entry); err != nil {
			logger.Errorf("Failed to stream directory entry: %v", err)
		}
		return nil
	}
	return nil
}

func (idx *Index) GetDirInfo(dirInfo *os.File, stat os.FileInfo, realPath, adjustedPath, combinedPath string, recursive bool) (*iteminfo.FileInfo, error) {
	// Ensure combinedPath has exactly one trailing slash to prevent double slashes in subdirectory paths
	combinedPath = strings.TrimRight(combinedPath, "/") + "/"
	// Read directory contents
	files, err := dirInfo.Readdir(-1)
	if err != nil {
		return nil, err
	}

	var totalSize int64
	dirHidden := isHidden(stat)
	dirInode := inodeFromFileInfo(stat)

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
			dirMapKey := NormalizeIndexPath(dirPath)

			if recursive {
				if err := idx.indexDirectory(dirPath, recursive); err != nil {
					logger.Debugf("Failed to %v", err)
					continue
				}
			}

			idx.mu.RLock()
			if meta, exists := idx.dirMetadata[dirMapKey]; exists {
				totalSize += meta.Size
			}
			idx.mu.RUnlock()

			if recursive {
				idx.NumDirs++
			}
		} else {
			size, shouldCountSize := idx.handleFile(file, fullCombined)
			if shouldCountSize {
				totalSize += int64(size)
			}
			if recursive {
				idx.NumFiles++
			}

			if recursive {
				normalized := NormalizeIndexPath(adjustedPath)
				childPath := makeChildRelativePath(normalized, baseName)
				absPath := filepath.Join(realPath, baseName)

				entry := IndexEntry{
					RelativePath: childPath,
					AbsolutePath: absPath,
					Name:         baseName,
					Size:         int64(size),
					ModTime:      file.ModTime(),
					Type:         "file",
					Hidden:       hidden,
					IsDir:        false,
					Inode:        inodeFromFileInfo(file),
				}
				if err := idx.streamWriter.Write(entry); err != nil {
					logger.Errorf("Failed to stream file entry: %v", err)
				}
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

func (idx *Index) SetStatus(status IndexStatus) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.Status = status
}

func (idx *Index) GetTotalSize() uint64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.totalSize
}

// Cleanup releases memory from temporary indexing structures after indexing completes.
// This helps reduce memory usage, especially for large filesystem scans.
func (idx *Index) Cleanup() {
	idx.mu.Lock()
	idx.processedInodes = nil
	idx.FoundHardLinks = nil
	idx.dirMetadata = nil
	idx.mu.Unlock()
}

func (idx *Index) isLinuxSystemPath(fullCombined string) bool {
	realPath := idx.realPathFromCombined(fullCombined)
	if realPath == "" {
		return false
	}

	for _, protectedPath := range linuxSystemPaths {
		protectedPath = filepath.Clean(protectedPath)
		if realPath == protectedPath {
			return true
		}
		if strings.HasPrefix(realPath, protectedPath+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (idx *Index) isExternalMount(fullCombined string) bool {
	if runtime.GOOS != "linux" {
		return false
	}

	realPath := idx.realPathFromCombined(fullCombined)
	if realPath == "" {
		return false
	}

	mounts := getExternalMountPoints()
	if len(mounts) == 0 {
		return false
	}

	path := filepath.Clean(realPath)
	for mountPoint := range mounts {
		if mountPoint == "" || mountPoint == "/" {
			continue
		}
		if path == mountPoint || strings.HasPrefix(path, mountPoint+string(os.PathSeparator)) {
			return true
		}
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

func getExternalMountPoints() map[string]string {
	externalMountsOnce.Do(func() {
		externalMountPoints = loadExternalMountPoints()
	})
	if externalMountPoints == nil {
		return map[string]string{}
	}
	return externalMountPoints
}

func loadExternalMountPoints() map[string]string {
	mounts := make(map[string]string)
	if runtime.GOOS != "linux" {
		return mounts
	}

	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		logger.Warnf("unable to read mountinfo: %v", err)
		return mounts
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		mountPoint, fsType, source, ok := parseMountInfo(scanner.Text())
		if !ok {
			continue
		}
		if isExternalFilesystem(fsType, source) {
			mounts[mountPoint] = fsType
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("error while scanning mountinfo: %v", err)
	}
	return mounts
}

func parseMountInfo(line string) (mountPoint, fsType, source string, ok bool) {
	parts := strings.Split(line, " - ")
	if len(parts) != 2 {
		return "", "", "", false
	}

	pre := strings.Fields(parts[0])
	post := strings.Fields(parts[1])
	if len(pre) < 5 || len(post) < 2 {
		return "", "", "", false
	}

	rawMountPoint := pre[4]
	mountPoint = decodeMountPath(rawMountPoint)
	mountPoint = filepath.Clean(mountPoint)
	fsType = strings.ToLower(post[0])
	source = strings.ToLower(post[1])
	return mountPoint, fsType, source, true
}

func decodeMountPath(raw string) string {
	decoded := strings.ReplaceAll(raw, "\\040", " ")
	decoded = strings.ReplaceAll(decoded, "\\011", "\t")
	decoded = strings.ReplaceAll(decoded, "\\012", "\n")
	decoded = strings.ReplaceAll(decoded, "\\134", "\\")
	return decoded
}

func isExternalFilesystem(fsType, source string) bool {
	if _, ok := externalFSTypes[fsType]; ok {
		return true
	}
	if strings.Contains(fsType, "iscsi") || strings.Contains(source, "iscsi") {
		return true
	}
	if strings.HasPrefix(source, "//") {
		return true
	}
	return false
}

func (idx *Index) handleFile(file os.FileInfo, fullCombined string) (size uint64, shouldCountSize bool) {
	var realSize uint64
	var nlink uint64 = 1
	var ino uint64 = 0
	canUseSyscall := false

	if sys := file.Sys(); sys != nil {
		realSize, nlink, ino, canUseSyscall = getFileDetails(sys)
	}

	if !canUseSyscall {
		// Fallback for non-unix systems or if syscall info is unavailable
		realSize = uint64(file.Size())
	}

	if nlink > 1 {
		idx.mu.Lock()
		if _, exists := idx.processedInodes[ino]; exists {
			idx.mu.Unlock()
			return realSize, false
		}
		idx.processedInodes[ino] = struct{}{}
		idx.FoundHardLinks[fullCombined] = realSize
		idx.totalSize += realSize
		idx.mu.Unlock()
		return realSize, true
	}

	// It's a regular file.
	idx.mu.Lock()
	idx.totalSize += realSize
	idx.mu.Unlock()
	return realSize, true // Count size.
}

func inodeFromFileInfo(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	_, _, ino, ok := getFileDetails(info.Sys())
	if ok {
		return ino
	}
	return 0
}

func getFileDetails(sys any) (uint64, uint64, uint64, bool) {
	if stat, ok := sys.(*syscall.Stat_t); ok {
		// Use allocated size for `du`-like behavior
		realSize := uint64(stat.Blocks * 512)
		return realSize, uint64(stat.Nlink), stat.Ino, true
	}
	return 0, 1, 0, false
}
