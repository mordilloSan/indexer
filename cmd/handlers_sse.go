package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mordilloSan/go-logger/logger"

	"github.com/mordilloSan/indexer/indexing"
	"github.com/mordilloSan/indexer/storage"
)

// SSEWriter wraps an http.ResponseWriter for Server-Sent Events
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter creates a new SSE writer and sets appropriate headers
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering

	return &SSEWriter{w: w, flusher: flusher}, nil
}

// SendEvent sends an SSE event with the given event type and data
func (s *SSEWriter) SendEvent(event string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, jsonData)
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// SendError sends an error event
func (s *SSEWriter) SendError(msg string) error {
	return s.SendEvent("error", map[string]string{"message": msg})
}

// ProgressEvent represents a progress update during indexing
type ProgressEvent struct {
	FilesIndexed int64  `json:"files_indexed"`
	DirsIndexed  int64  `json:"dirs_indexed"`
	CurrentPath  string `json:"current_path,omitempty"`
	BytesIndexed int64  `json:"bytes_indexed,omitempty"`
}

// ReindexCompleteEvent represents completion of a reindex operation
type ReindexCompleteEvent struct {
	Path         string `json:"path"`
	FilesIndexed int64  `json:"files_indexed"`
	DirsIndexed  int64  `json:"dirs_indexed"`
	TotalSize    int64  `json:"total_size"`
	DurationMs   int64  `json:"duration_ms"`
}

// VacuumProgressEvent represents vacuum progress
type VacuumProgressEvent struct {
	Phase   string `json:"phase"`
	Message string `json:"message,omitempty"`
}

// VacuumCompleteEvent represents completion of vacuum operation
type VacuumCompleteEvent struct {
	DurationMs int64 `json:"duration_ms"`
}

// handleReindexStream handles POST /reindex/stream with SSE progress updates
func (d *daemon) handleReindexStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	// Reject path traversal attempts
	if !indexing.ValidateRelativePath(path) {
		http.Error(w, "invalid path: path traversal not allowed", http.StatusBadRequest)
		return
	}

	normalizedPath := indexing.NormalizeIndexPath(path)

	if !d.tryLockIndex() {
		http.Error(w, "indexer already running", http.StatusConflict)
		return
	}

	sse, err := NewSSEWriter(w)
	if err != nil {
		d.unlockIndex()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send initial event
	_ = sse.SendEvent("started", map[string]string{
		"status": "running",
		"path":   normalizedPath,
	})

	// Run reindex with progress callback
	go func() {
		defer d.unlockIndex()
		d.reindexPathWithProgress(r.Context(), normalizedPath, sse)
	}()

	// Keep connection open until context is cancelled or operation completes
	<-r.Context().Done()
}

// reindexPathWithProgress reindexes a path and sends progress events via SSE
func (d *daemon) reindexPathWithProgress(ctx context.Context, relativePath string, sse *SSEWriter) {
	start := time.Now()
	logger.Infof("Starting streaming reindex for path: %s", relativePath)

	// Get the latest index ID
	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil {
		_ = sse.SendError(fmt.Sprintf("no index present: %v", err))
		return
	}

	// Send progress: deleting old entries
	_ = sse.SendEvent("progress", ProgressEvent{CurrentPath: "Deleting old entries..."})

	// Delete all entries under this path
	if err := storage.DeletePathRecursive(ctx, d.db, indexID, relativePath); err != nil {
		_ = sse.SendError(fmt.Sprintf("failed to delete existing entries: %v", err))
		return
	}

	// Create index instance
	index := indexing.Initialize(d.cfg.IndexName, d.cfg.IndexPath, d.cfg.IndexPath, d.cfg.IncludeHidden)

	// Create streaming writer with progress callback
	writer := storage.NewStreamingWriterWithProgress(ctx, d.db, indexID, 1000, func(filesWritten, dirsWritten int64, lastPath string) {
		// Send progress every 100 entries to avoid overwhelming the client
		if (filesWritten+dirsWritten)%100 == 0 {
			_ = sse.SendEvent("progress", ProgressEvent{
				FilesIndexed: filesWritten,
				DirsIndexed:  dirsWritten,
				CurrentPath:  lastPath,
			})
		}
	})
	index.EnableStreaming(writer)

	// Start indexing
	if err := index.StartIndexingFromPath(relativePath); err != nil {
		_ = writer.Close()
		_ = sse.SendError(fmt.Sprintf("indexing failed: %v", err))
		return
	}

	// Flush remaining batches
	if err := writer.Close(); err != nil {
		_ = sse.SendError(fmt.Sprintf("streaming writer close: %v", err))
		return
	}

	// Cleanup deleted entries
	scanTime := writer.ScanTime()
	deleted, err := storage.CleanupDeletedEntriesUnderPath(ctx, d.db, indexID, relativePath, scanTime)
	if err != nil {
		_ = sse.SendError(fmt.Sprintf("cleanup deleted entries: %v", err))
		return
	}
	if deleted > 0 {
		logger.Infof("Cleaned up %d deleted entries under %s", deleted, relativePath)
	}

	// Get the new total size
	var newSize int64
	err = d.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size), 0)
		FROM entries
		WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?);
	`, indexID, relativePath, relativePath+"/%").Scan(&newSize)
	if err != nil {
		_ = sse.SendError(fmt.Sprintf("query new size: %v", err))
		return
	}

	// Update parent sizes
	if err := storage.UpdateParentDirectorySizes(ctx, d.db, indexID, relativePath, newSize); err != nil {
		_ = sse.SendError(fmt.Sprintf("update parent sizes: %v", err))
		return
	}

	// WAL checkpoint
	if stats, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("WAL checkpoint failed after reindex: %v", err)
	} else {
		logger.Infof("WAL checkpoint complete after reindex in %v", stats.Duration)
	}
	_ = storage.ReleaseSQLiteMemory(ctx, d.db)

	duration := time.Since(start)
	logger.Infof("Streaming reindex complete for path %s in %v", relativePath, duration)

	// Send completion event
	_ = sse.SendEvent("complete", ReindexCompleteEvent{
		Path:         relativePath,
		FilesIndexed: int64(index.NumFiles),
		DirsIndexed:  int64(index.NumDirs),
		TotalSize:    newSize,
		DurationMs:   duration.Milliseconds(),
	})
}

// handleVacuumStream handles POST /vacuum/stream with SSE progress updates
func (d *daemon) handleVacuumStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	if !d.tryLockIndex() {
		http.Error(w, "indexer already running", http.StatusConflict)
		return
	}

	sse, err := NewSSEWriter(w)
	if err != nil {
		d.unlockIndex()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send initial event
	_ = sse.SendEvent("started", map[string]string{"status": "running"})

	// Run vacuum with progress updates
	go func() {
		defer d.unlockIndex()
		d.vacuumWithProgress(r.Context(), sse)
	}()

	// Keep connection open
	<-r.Context().Done()
}

// vacuumWithProgress runs vacuum and sends progress events via SSE
func (d *daemon) vacuumWithProgress(ctx context.Context, sse *SSEWriter) {
	start := time.Now()

	ctx, cancel := context.WithTimeout(ctx, 1*time.Hour)
	defer cancel()

	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logger.Warnf("vacuum: latest index id unavailable: %v", err)
		indexID = 0
	}

	// Phase 1: Pre-checkpoint
	_ = sse.SendEvent("progress", VacuumProgressEvent{
		Phase:   "pre_checkpoint",
		Message: "Running WAL checkpoint before vacuum...",
	})

	if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("vacuum: wal checkpoint (pre) failed: %v", err)
		_ = sse.SendEvent("progress", VacuumProgressEvent{
			Phase:   "pre_checkpoint",
			Message: fmt.Sprintf("WAL checkpoint warning: %v", err),
		})
	}

	// Phase 2: Vacuum
	_ = sse.SendEvent("progress", VacuumProgressEvent{
		Phase:   "vacuum",
		Message: "Running VACUUM (this may take a while)...",
	})

	vs, err := storage.Vacuum(ctx, d.db)
	if err != nil {
		logger.Errorf("vacuum failed: %v", err)
		_ = sse.SendError(fmt.Sprintf("vacuum failed: %v", err))
		return
	}

	logger.Infof("Vacuum complete in %v", vs.Duration)

	// Phase 3: Post-checkpoint
	_ = sse.SendEvent("progress", VacuumProgressEvent{
		Phase:   "post_checkpoint",
		Message: "Running WAL checkpoint after vacuum...",
	})

	if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("vacuum: wal checkpoint (post) failed: %v", err)
	}
	_ = storage.ReleaseSQLiteMemory(ctx, d.db)

	// Update duration metadata
	if indexID != 0 {
		if _, err := d.db.ExecContext(ctx, `UPDATE indexes SET vacuum_duration_ms = ? WHERE id = ?;`, vs.Duration.Milliseconds(), indexID); err != nil {
			logger.Warnf("vacuum: failed to persist duration: %v", err)
		}
	}

	duration := time.Since(start)

	// Send completion event
	_ = sse.SendEvent("complete", VacuumCompleteEvent{
		DurationMs: duration.Milliseconds(),
	})
}
