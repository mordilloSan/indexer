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
	ErrNotIndexed = errors.New("path not indexed")
)

// actionConfig holds all configuration options for indexing operations
type actionConfig struct {
	Quick     bool // whether to perform a quick scan (skip unchanged directories)
	Recursive bool // whether to recursively index subdirectories
}

// reduced index is json exposed to the client
type ReducedIndex struct {
	IdxName         string      `json:"name"`
	DiskUsed        uint64      `json:"used"`
	DiskTotal       uint64      `json:"total"`
	Status          IndexStatus `json:"status"`
	NumDirs         uint64      `json:"numDirs"`
	NumFiles        uint64      `json:"numFiles"`
	NumDeleted      uint64      `json:"numDeleted"`
	LastIndexed     time.Time   `json:"-"`
	LastIndexedUnix int64       `json:"lastIndexedUnixTime"`
	QuickScanTime   int         `json:"quickScanDurationSeconds"`
	FullScanTime    int         `json:"fullScanDurationSeconds"`
	Assessment      string      `json:"assessment"`
}
type Index struct {
	ReducedIndex
	Name                       string                        // unique name for this index
	Path                       string                        // filesystem path being indexed
	Source                     string                        // source identifier
	includeHidden              bool                          // whether to include hidden files and directories
	Directories                map[string]*iteminfo.FileInfo `json:"-"` // indexed directories
	DirectoriesLedger          map[string]struct{}           `json:"-"` // set of indexed paths
	FilesChangedDuringIndexing bool                          `json:"-"` // whether files changed during indexing
	wasIndexed                 bool                          `json:"-"` // whether initial indexing is complete
	FoundHardLinks             map[string]uint64             `json:"-"` // hardlink path -> size
	processedInodes            map[uint64]struct{}           `json:"-"` // tracks processed inodes for hardlinks
	totalSize                  uint64                        `json:"-"` // total size
	mu                         sync.RWMutex                  `json:"-"` // protects concurrent access
	quickScan                  bool                          `json:"-"`
}

var (
	indexes      map[string]*Index
	indexesMutex sync.RWMutex
)

type IndexStatus string

const (
	READY       IndexStatus = "ready"
	INDEXING    IndexStatus = "indexing"
	UNAVAILABLE IndexStatus = "unavailable"
)

// SearchResult represents a search match
type SearchResult struct {
	Path  string // full path to the file/folder
	Name  string // name of the file/folder
	Size  int64  // size in bytes
	IsDir bool   // whether it's a directory
}

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

func init() {
	indexes = make(map[string]*Index)
}

// Initialize creates a new index for the given path
// name: a unique name for this index
// path: the filesystem path to index (e.g., "/", "/home", "/home/user/documents")
// source: an optional source identifier (can be same as name)
// includeHidden: whether to include hidden files and directories (starting with .)
func Initialize(name string, path string, source string, includeHidden bool) *Index {
	indexesMutex.Lock()
	defer indexesMutex.Unlock()

	newIndex := &Index{
		Name:              name,
		Path:              path,
		Source:            source,
		includeHidden:     includeHidden,
		Directories:       make(map[string]*iteminfo.FileInfo),
		DirectoriesLedger: make(map[string]struct{}),
		processedInodes:   make(map[uint64]struct{}),
		FoundHardLinks:    make(map[string]uint64),
	}
	newIndex.ReducedIndex = ReducedIndex{
		Status:     READY,
		IdxName:    name,
		Assessment: "not indexed",
	}
	indexes[name] = newIndex

	logger.Infof("initialized index [%s] for path [%s] (includeHidden: %v)", name, path, includeHidden)
	return newIndex
}

// ApplySnapshot seeds the index with previously stored directory information
// so subsequent indexing can perform quick scans against unchanged paths.
func (idx *Index) ApplySnapshot(dirs map[string]*iteminfo.FileInfo) {
	if len(dirs) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.Directories = dirs
	idx.DirectoriesLedger = make(map[string]struct{}, len(dirs))
	for path := range dirs {
		idx.DirectoriesLedger[path] = struct{}{}
	}
	idx.quickScan = true
}

// RefreshAbsolutePath re-indexes a specific filesystem path (file or directory).
// For files, the containing directory is refreshed.
func (idx *Index) RefreshAbsolutePath(absPath string, recursive bool) error {
	if absPath == "" {
		return fmt.Errorf("path cannot be empty")
	}

	cleanRoot := filepath.Clean(idx.Path)
	target, err := filepath.Abs(absPath)
	if err != nil {
		return err
	}
	if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
		return fmt.Errorf("path %s is outside indexed root %s", target, cleanRoot)
	}

	info, statErr := os.Stat(target)
	isDir := statErr == nil && info.IsDir()
	indexPath := idx.MakeIndexPath(target)

	if statErr != nil {
		if os.IsNotExist(statErr) {
			parent := filepath.Dir(target)
			indexPath = idx.MakeIndexPath(parent)
			isDir = true
		} else {
			return statErr
		}
	}

	if !isDir {
		indexPath = strings.TrimSuffix(indexPath, "/")
	}

	opts := iteminfo.FileOptions{
		Path:      indexPath,
		IsDir:     isDir,
		Recursive: recursive && isDir,
	}
	return idx.RefreshFileInfo(opts)
}

// StartIndexing begins indexing the configured path
func (idx *Index) StartIndexing() error {
	idx.SetStatus(INDEXING)
	logger.Infof("starting indexing for [%s] at path [%s]", idx.Name, idx.Path)

	idx.mu.Lock()
	quick := idx.quickScan
	idx.quickScan = false
	idx.mu.Unlock()

	config := actionConfig{
		Quick:     quick,
		Recursive: true,
	}

	err := idx.indexDirectory("/", config)
	if err != nil {
		idx.SetStatus(UNAVAILABLE)
		return err
	}

	idx.wasIndexed = true
	idx.SetStatus(READY)
	logger.Infof("completed indexing for [%s]: %d directories, %d files", idx.Name, idx.NumDirs, idx.NumFiles)
	return nil
}

// indexDirectoryWithOptions wraps indexDirectory with actionConfig
func (idx *Index) indexDirectoryWithOptions(adjustedPath string, config actionConfig) error {
	return idx.indexDirectory(adjustedPath, config)
}

// Define a function to recursively index files and directories
func (idx *Index) indexDirectory(adjustedPath string, config actionConfig) error {
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

	// if indexing, mark the directory as valid and indexed.
	if config.Recursive {
		// Prevent race conditions if scanning becomes concurrent in the future.
		idx.mu.Lock()
		idx.DirectoriesLedger[adjustedPath] = struct{}{}
		idx.mu.Unlock()
	}
	// adjustedPath is already normalized with trailing slash
	combinedPath := adjustedPath
	// get whats currently in cache
	idx.mu.RLock()
	cacheDirItems := []iteminfo.ItemInfo{}
	modChange := false
	cachedDir, exists := idx.Directories[adjustedPath]
	if exists {
		modChange = dirInfo.ModTime() != cachedDir.ModTime
		cacheDirItems = cachedDir.Folders
	}
	idx.mu.RUnlock()

	// If the directory has not been modified since the last index, skip expensive readdir
	// recursively check cached dirs for mod time changes as well
	if config.Recursive {
		if modChange {
			idx.mu.Lock()
			idx.FilesChangedDuringIndexing = true
			idx.mu.Unlock()
		} else if config.Quick {
			for _, item := range cacheDirItems {
				subConfig := actionConfig{
					Quick:     config.Quick,
					Recursive: true,
				}
				err = idx.indexDirectory(combinedPath+item.Name, subConfig)
				if err != nil && err != ErrNotIndexed {
					logger.Errorf("error indexing directory %v : %v", combinedPath+item.Name, err)
				}
			}
			return nil
		}
	}
	dirFileInfo, err2 := idx.GetDirInfo(dir, dirInfo, realPath, adjustedPath, combinedPath, config)
	if err2 != nil {
		return err2
	}
	// Store the directory info in the index
	idx.mu.Lock()
	idx.Directories[adjustedPath] = dirFileInfo
	idx.mu.Unlock()
	return nil
}

func (idx *Index) GetFsDirInfo(adjustedPath string) (*iteminfo.FileInfo, error) {
	realPath, isDir, err := idx.GetRealPath(adjustedPath)
	if err != nil {
		return nil, err
	}
	originalPath := realPath

	dir, err := os.Open(realPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()

	dirInfo, err := dir.Stat()
	if err != nil {
		return nil, err
	}

	if !dirInfo.IsDir() {
		fileInfo := iteminfo.FileInfo{
			Path: adjustedPath,
			ItemInfo: iteminfo.ItemInfo{
				Name:    filepath.Base(originalPath),
				Size:    dirInfo.Size(),
				ModTime: dirInfo.ModTime(),
				Type:    "file",
			},
		}

		return &fileInfo, nil
	}

	// Normalize directory path to always have trailing slash
	if adjustedPath != "/" {
		adjustedPath = strings.TrimSuffix(adjustedPath, "/") + "/"
	}
	// adjustedPath is already normalized with trailing slash
	combinedPath := adjustedPath
	var response *iteminfo.FileInfo
	response, err = idx.GetDirInfo(dir, dirInfo, realPath, adjustedPath, combinedPath, actionConfig{
		Quick:     false,
		Recursive: false,
	})
	if err != nil {
		return nil, err
	}
	if !isDir {
		baseName := filepath.Base(originalPath)
		idx.MakeIndexPath(realPath)
		found := false
		for _, item := range response.Files {
			if item.Name == baseName {
				// Clean path to remove trailing slashes before joining
				filePath := strings.TrimSuffix(adjustedPath, "/") + "/" + item.Name
				response = &iteminfo.FileInfo{
					Path:     filePath,
					ItemInfo: item,
				}
				found = true
				continue
			}
		}
		if !found {
			return nil, fmt.Errorf("file not found in directory: %s", adjustedPath)
		}

	}
	return response, nil

}

func (idx *Index) GetDirInfo(dirInfo *os.File, stat os.FileInfo, realPath, adjustedPath, combinedPath string, config actionConfig) (*iteminfo.FileInfo, error) {
	// Ensure combinedPath has exactly one trailing slash to prevent double slashes in subdirectory paths
	combinedPath = strings.TrimRight(combinedPath, "/") + "/"
	// Read directory contents
	files, err := dirInfo.Readdir(-1)
	if err != nil {
		return nil, err
	}
	var totalSize int64
	fileInfos := []iteminfo.ItemInfo{}
	dirInfos := []iteminfo.ItemInfo{}

	// Process each file and directory in the current directory
	for _, file := range files {
		hidden := isHidden(file)
		isDir := iteminfo.IsDirectory(file)
		baseName := file.Name()
		fullCombined := combinedPath + baseName
		// Skip items based on simple rules (hidden files, system directories)
		if idx.shouldSkip(isDir, hidden, fullCombined) {
			continue
		}
		itemInfo := &iteminfo.ItemInfo{
			Name:    file.Name(),
			ModTime: file.ModTime(),
			Hidden:  hidden,
		}
		itemInfo.Inode = inodeFromFileInfo(file)

		if isDir {
			dirPath := combinedPath + file.Name()
			dirMapKey := normalizeIndexPath(dirPath)

			if config.Recursive {
				// Recursively index the subdirectory
				err = idx.indexDirectory(dirPath, config)
				if err != nil {
					logger.Debugf("Failed to %v", err)
					continue
				}
			}

			// Get the size from the indexed directory
			idx.mu.RLock()
			if realDirInfo, exists := idx.Directories[dirMapKey]; exists {
				itemInfo.Size = realDirInfo.Size
				itemInfo.Inode = realDirInfo.Inode
			}
			idx.mu.RUnlock()

			totalSize += itemInfo.Size
			itemInfo.Type = "directory"
			dirInfos = append(dirInfos, *itemInfo)
			if config.Recursive {
				idx.NumDirs++
			}
		} else {
			size, shouldCountSize := idx.handleFile(file, fullCombined)
			itemInfo.Type = "file"
			itemInfo.Size = int64(size)

			fileInfos = append(fileInfos, *itemInfo)
			if shouldCountSize {
				totalSize += itemInfo.Size
			}
			if config.Recursive {
				idx.NumFiles++
			}
		}
	}

	// Optionally skip empty folders
	// if totalSize == 0 {
	// 	return nil, ErrNotIndexed
	// }

	if adjustedPath == "/" {
		idx.mu.Lock()
		idx.DiskUsed = uint64(totalSize)
		idx.mu.Unlock()
	}

	// Create FileInfo for the current directory (adjustedPath is already normalized with trailing slash)
	dirFileInfo := &iteminfo.FileInfo{
		Path:    adjustedPath,
		Files:   fileInfos,
		Folders: dirInfos,
	}
	dirFileInfo.ItemInfo = iteminfo.ItemInfo{
		Name:    filepath.Base(dirInfo.Name()),
		Type:    "directory",
		Size:    totalSize,
		ModTime: stat.ModTime(),
		Inode:   inodeFromFileInfo(stat),
	}
	dirFileInfo.SortItems()

	// Metadata will be updated by the caller (indexDirectory or GetFsDirInfo)
	return dirFileInfo, nil
}

func (idx *Index) recursiveUpdateDirSizes(childInfo *iteminfo.FileInfo, previousSize int64) {
	parentDir := iteminfo.GetParentDirectoryPath(childInfo.Path)

	if parentDir == "" {
		return
	}

	idx.mu.Lock()
	defer idx.mu.Unlock()

	parentInfo, exists := idx.Directories[parentDir]
	if !exists {
		return
	}

	// Calculate size delta and update parent
	previousParentSize := parentInfo.Size
	sizeDelta := childInfo.Size - previousSize
	parentInfo.Size = previousParentSize + sizeDelta

	idx.Directories[parentDir] = parentInfo

	// Note: We unlock here before recursion to avoid deadlock
	idx.mu.Unlock()
	idx.recursiveUpdateDirSizes(parentInfo, previousParentSize)
	idx.mu.Lock() // Re-lock for the defer
}

func (idx *Index) GetRealPath(relativePath ...string) (string, bool, error) {
	combined := append([]string{idx.Path}, relativePath...)
	joinedPath := filepath.Join(combined...)

	// Convert relative path to absolute path
	absolutePath, err := filepath.Abs(joinedPath)
	if err != nil {
		return absolutePath, false, fmt.Errorf("could not get real path: %v, %s", joinedPath, err)
	}

	// Resolve symlinks and get the real path
	realPath, isDir, err := iteminfo.ResolveSymlinks(absolutePath)
	return realPath, isDir, err
}

func (idx *Index) RefreshFileInfo(opts iteminfo.FileOptions) error {
	config := actionConfig{
		Quick:     false,
		Recursive: opts.Recursive,
	}

	targetPath := opts.Path
	if !opts.IsDir {
		targetPath = idx.MakeIndexPath(filepath.Dir(targetPath))
	}

	// Get PREVIOUS size BEFORE indexing
	var previousSize int64
	idx.mu.RLock()
	if previousInfo, exists := idx.Directories[targetPath]; exists {
		previousSize = previousInfo.Size
	}
	idx.mu.RUnlock()

	// Re-index the directory
	err := idx.indexDirectoryWithOptions(targetPath, config)
	if err != nil {
		return err
	}

	// Get the NEW size after indexing
	idx.mu.RLock()
	newInfo, exists := idx.Directories[targetPath]
	idx.mu.RUnlock()

	if !exists {
		return fmt.Errorf("file/folder does not exist: %s", targetPath)
	}

	// If size changed, propagate to parents
	if previousSize != newInfo.Size {
		idx.recursiveUpdateDirSizes(newInfo, previousSize)
	}

	return nil
}

func isHidden(file os.FileInfo) bool {
	// Check if the file starts with a dot (Linux hidden files)
	if file.Name()[0] == '.' {
		return true
	}
	return false
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

func (idx *Index) SetUsage(totalBytes uint64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.DiskTotal = totalBytes
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

// Search finds all files and folders matching the search term
func (idx *Index) Search(searchTerm string, caseSensitive bool) []SearchResult {
	searchOpts := iteminfo.SearchOptions{
		CaseSensitive: caseSensitive,
		Terms:         []string{searchTerm},
	}

	results := []SearchResult{}
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for path, dirInfo := range idx.Directories {
		// Search in files
		for _, file := range dirInfo.Files {
			if file.ContainsSearchTerm(searchTerm, searchOpts) {
				fullPath := path
				if !strings.HasSuffix(fullPath, "/") {
					fullPath += "/"
				}
				fullPath += file.Name
				results = append(results, SearchResult{
					Path:  fullPath,
					Name:  file.Name,
					Size:  file.Size,
					IsDir: false,
				})
			}
		}

		// Search in folders
		for _, folder := range dirInfo.Folders {
			if folder.ContainsSearchTerm(searchTerm, searchOpts) {
				fullPath := path
				if !strings.HasSuffix(fullPath, "/") {
					fullPath += "/"
				}
				fullPath += folder.Name
				results = append(results, SearchResult{
					Path:  fullPath,
					Name:  folder.Name,
					Size:  folder.Size,
					IsDir: true,
				})
			}
		}
	}

	return results
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
		// It's a hard link
		idx.mu.Lock()
		defer idx.mu.Unlock()
		if _, exists := idx.processedInodes[ino]; exists {
			// Already seen, don't count towards global total, or directory total.
			return realSize, false
		}
		// First time seeing this inode.
		idx.processedInodes[ino] = struct{}{}
		idx.FoundHardLinks[fullCombined] = realSize
		idx.totalSize += realSize
		return realSize, true // Count size for directory total.
	}

	// It's a regular file.
	idx.mu.Lock()
	idx.totalSize += realSize
	idx.mu.Unlock()
	return realSize, true // Count size.
}

// input should be non-index path.
func (idx *Index) MakeIndexPath(path string) string {
	if path == "." || strings.HasPrefix(path, "./") {
		path = strings.TrimPrefix(path, ".")
	}
	path = strings.TrimPrefix(path, idx.Path)
	// Normalize to a canonical Linux-style index path:
	// - ensure a single leading "/"
	// - ensure a trailing "/" so directories look like "/foo/"
	path = "/" + strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/") + "/"
	return path
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

func normalizeIndexPath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	path = strings.TrimSuffix(path, "/") + "/"
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func getFileDetails(sys any) (uint64, uint64, uint64, bool) {
	if stat, ok := sys.(*syscall.Stat_t); ok {
		// Use allocated size for `du`-like behavior
		realSize := uint64(stat.Blocks * 512)
		return realSize, uint64(stat.Nlink), stat.Ino, true
	}
	return 0, 1, 0, false
}
