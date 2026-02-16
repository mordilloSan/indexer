package indexing

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mordilloSan/go-logger/logger"
)

var (
	// ErrNotIndexed is returned when a path is not indexed.
	ErrNotIndexed = errors.New("path not indexed")
)

// ReducedIndex is the JSON-friendly representation exposed to clients.
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

// Initialize creates a new index for the given path.
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

// StartIndexing begins indexing the configured path.
func (idx *Index) StartIndexing() error {
	idx.mu.RLock()
	writer := idx.streamWriter
	idx.mu.RUnlock()

	if writer == nil {
		return fmt.Errorf("streaming mode is required; call EnableStreaming with a writer before indexing")
	}

	idx.SetStatus(INDEXING)
	logger.Infof("starting indexing for [%s] at path [%s]", idx.Name, idx.Path)

	err := idx.indexDirectory("/")
	if err != nil {
		idx.SetStatus(UNAVAILABLE)
		return err
	}

	idx.SetStatus(READY)
	idx.mu.RLock()
	dirs := idx.NumDirs
	files := idx.NumFiles
	idx.mu.RUnlock()
	logger.Infof("completed indexing for [%s]: %d directories, %d files", idx.Name, dirs, files)
	return nil
}

// StartIndexingFromPath begins indexing from a specific subdirectory within the configured path.
// relativePath should be a normalized path like "/home/user" relative to the index root.
func (idx *Index) StartIndexingFromPath(relativePath string) error {
	idx.mu.RLock()
	writer := idx.streamWriter
	idx.mu.RUnlock()

	if writer == nil {
		return fmt.Errorf("streaming mode is required; call EnableStreaming with a writer before indexing")
	}

	logger.Infof("starting partial reindex for [%s] at subpath [%s]", idx.Name, relativePath)

	// Normalize the path
	normalizedPath := NormalizeIndexPath(relativePath)

	err := idx.indexDirectory(normalizedPath)
	if err != nil {
		return err
	}

	idx.mu.RLock()
	dirs := idx.NumDirs
	files := idx.NumFiles
	idx.mu.RUnlock()
	logger.Infof("completed partial reindex for [%s]: %d directories, %d files", idx.Name, dirs, files)
	return nil
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
	idx.streamWriter = nil
	idx.mu.Unlock()
}
