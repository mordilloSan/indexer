package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mordilloSan/go-logger/logger"

	"github.com/mordilloSan/indexer/indexing"
	"github.com/mordilloSan/indexer/storage"
)

// SSEWriter wraps an http.ResponseWriter for Server-Sent Events.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewSSEWriter creates a new SSE writer and sets appropriate headers.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	return &SSEWriter{w: w, flusher: flusher}, nil
}

// SendEvent sends an SSE event with the given event type and payload.
func (s *SSEWriter) SendEvent(event string, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}

	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, jsonData); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// SendError sends an SSE error event.
func (s *SSEWriter) SendError(msg string) error {
	return s.SendEvent("error", map[string]string{"message": msg})
}

type sseEventSender interface {
	SendEvent(event string, data any) error
	SendError(msg string) error
}

type workStreamEvent struct {
	event string
	data  any
}

type workStreamBroadcaster struct {
	operation        string
	path             string
	mu               sync.Mutex
	subscribers      map[int]chan workStreamEvent
	nextSubscriberID int
	closed           bool
}

const workStreamSubscriberBuffer = 64

func newWorkStreamBroadcaster(operation, path string) *workStreamBroadcaster {
	return &workStreamBroadcaster{
		operation:   operation,
		path:        path,
		subscribers: make(map[int]chan workStreamEvent),
	}
}

func (b *workStreamBroadcaster) Operation() string {
	return b.operation
}

func (b *workStreamBroadcaster) Path() string {
	return b.path
}

func (b *workStreamBroadcaster) SendEvent(event string, data any) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return fmt.Errorf("work stream closed")
	}

	evt := workStreamEvent{event: event, data: data}
	for id, ch := range b.subscribers {
		select {
		case ch <- evt:
		default:
			close(ch)
			delete(b.subscribers, id)
			logger.Warnf("Dropping slow work stream subscriber id=%d", id)
		}
	}

	return nil
}

func (b *workStreamBroadcaster) SendError(msg string) error {
	return b.SendEvent("error", map[string]string{"message": msg})
}

func (b *workStreamBroadcaster) subscribe() (int, <-chan workStreamEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, nil, fmt.Errorf("work stream closed")
	}

	id := b.nextSubscriberID
	b.nextSubscriberID++
	ch := make(chan workStreamEvent, workStreamSubscriberBuffer)
	b.subscribers[id] = ch

	return id, ch, nil
}

func (b *workStreamBroadcaster) unsubscribe(id int) {
	b.mu.Lock()
	ch, ok := b.subscribers[id]
	if ok {
		delete(b.subscribers, id)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *workStreamBroadcaster) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for id, ch := range b.subscribers {
		delete(b.subscribers, id)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *workStreamBroadcaster) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

func sendSSEEvent(s sseEventSender, event string, data any) bool {
	if err := s.SendEvent(event, data); err != nil {
		logger.Warnf("SSE send %q failed: %v", event, err)
		return false
	}
	return true
}

func sendSSEError(s sseEventSender, msg string) {
	if err := s.SendError(msg); err != nil {
		logger.Warnf("SSE send error event failed: %v", err)
	}
}

type WorkStartedEvent struct {
	Status    string `json:"status"`
	Operation string `json:"operation"`
	Path      string `json:"path,omitempty"`
}

type WorkProgressEvent struct {
	Operation    string `json:"operation"`
	Phase        string `json:"phase,omitempty"`
	Message      string `json:"message,omitempty"`
	FilesIndexed int64  `json:"files_indexed,omitempty"`
	DirsIndexed  int64  `json:"dirs_indexed,omitempty"`
	CurrentPath  string `json:"current_path,omitempty"`
	BytesIndexed int64  `json:"bytes_indexed,omitempty"`
}

type WorkCompleteEvent struct {
	Status         string `json:"status"`
	Operation      string `json:"operation"`
	Path           string `json:"path,omitempty"`
	DurationMs     int64  `json:"duration_ms"`
	FilesIndexed   int64  `json:"files_indexed,omitempty"`
	DirsIndexed    int64  `json:"dirs_indexed,omitempty"`
	TotalSize      int64  `json:"total_size,omitempty"`
	DeletedIndexes int    `json:"deleted_indexes,omitempty"`
	DeletedEntries int64  `json:"deleted_entries,omitempty"`
}

func wantsStatusSSE(r *http.Request) bool {
	streamParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stream")))
	if streamParam == "1" || streamParam == "true" || streamParam == "yes" {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
}

func (d *daemon) attachStatusSSEIfActive(w http.ResponseWriter, r *http.Request) bool {
	broadcaster := d.getWorkStreamBroadcaster()
	if broadcaster == nil {
		return false
	}

	sse, err := NewSSEWriter(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}

	subscriberID, events, err := broadcaster.subscribe()
	if err != nil {
		// Race with operation completion; fallback to JSON status response.
		return false
	}
	defer broadcaster.unsubscribe(subscriberID)

	if !sendSSEEvent(sse, "started", WorkStartedEvent{
		Status:    "running",
		Operation: broadcaster.Operation(),
		Path:      broadcaster.Path(),
	}) {
		return true
	}

	d.streamWorkEvents(r.Context(), sse, events)
	return true
}

func (d *daemon) streamWorkEvents(ctx context.Context, sse *SSEWriter, events <-chan workStreamEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			if !sendSSEEvent(sse, evt.event, evt.data) {
				return
			}
		}
	}
}

func (d *daemon) beginWorkStream(operation, path string) *workStreamBroadcaster {
	b := newWorkStreamBroadcaster(operation, path)
	d.setWorkStreamBroadcaster(b)
	return b
}

func (d *daemon) endWorkStream(b *workStreamBroadcaster) {
	d.clearWorkStreamBroadcaster(b)
	b.close()
}

// reindexPathWithProgress reindexes a path and emits progress events.
func (d *daemon) reindexPathWithProgress(ctx context.Context, relativePath string, sender sseEventSender) {
	start := time.Now()
	logger.Infof("Starting reindex for path: %s", relativePath)

	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil {
		sendSSEError(sender, fmt.Sprintf("no index present: %v", err))
		return
	}

	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation:   "reindex",
		CurrentPath: "Deleting old entries...",
	}) {
		return
	}

	if err := storage.DeletePathRecursive(ctx, d.db, indexID, relativePath); err != nil {
		sendSSEError(sender, fmt.Sprintf("failed to delete existing entries: %v", err))
		return
	}

	index := indexing.Initialize(d.cfg.IndexName, d.cfg.IndexPath, d.cfg.IndexPath, d.cfg.IncludeHidden)
	writer := storage.NewStreamingWriterWithProgress(ctx, d.db, indexID, 1000, func(filesWritten, dirsWritten int64, lastPath string) {
		if (filesWritten+dirsWritten)%100 == 0 {
			sendSSEEvent(sender, "progress", WorkProgressEvent{
				Operation:    "reindex",
				FilesIndexed: filesWritten,
				DirsIndexed:  dirsWritten,
				CurrentPath:  lastPath,
			})
		}
	})
	index.EnableStreaming(writer)

	if err := index.StartIndexingFromPath(relativePath); err != nil {
		if closeErr := writer.Close(); closeErr != nil {
			logger.Warnf("Failed to close streaming writer after reindex stream error: %v", closeErr)
		}
		sendSSEError(sender, fmt.Sprintf("indexing failed: %v", err))
		return
	}

	if err := writer.Close(); err != nil {
		sendSSEError(sender, fmt.Sprintf("streaming writer close: %v", err))
		return
	}

	scanTime := writer.ScanTime()
	deleted, err := storage.CleanupDeletedEntriesUnderPath(ctx, d.db, indexID, relativePath, scanTime)
	if err != nil {
		sendSSEError(sender, fmt.Sprintf("cleanup deleted entries: %v", err))
		return
	}
	if deleted > 0 {
		logger.Infof("Cleaned up %d deleted entries under %s", deleted, relativePath)
	}

	var newSize int64
	err = d.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(size), 0)
		FROM entries
		WHERE index_id = ? AND (relative_path = ? OR relative_path LIKE ?);
	`, indexID, relativePath, relativePath+"/%").Scan(&newSize)
	if err != nil {
		sendSSEError(sender, fmt.Sprintf("query new size: %v", err))
		return
	}

	if err := storage.UpdateParentDirectorySizes(ctx, d.db, indexID, relativePath, newSize); err != nil {
		sendSSEError(sender, fmt.Sprintf("update parent sizes: %v", err))
		return
	}

	if stats, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("WAL checkpoint failed after reindex: %v", err)
	} else {
		logger.Infof("WAL checkpoint complete after reindex in %v", stats.Duration)
	}
	if err := storage.ReleaseSQLiteMemory(ctx, d.db); err != nil {
		logger.Warnf("Failed to release SQLite memory after reindex: %v", err)
	}

	duration := time.Since(start)
	logger.Infof("Reindex complete for path %s in %v", relativePath, duration)

	sendSSEEvent(sender, "complete", WorkCompleteEvent{
		Status:       "complete",
		Operation:    "reindex",
		Path:         relativePath,
		FilesIndexed: int64(index.NumFiles),
		DirsIndexed:  int64(index.NumDirs),
		TotalSize:    newSize,
		DurationMs:   duration.Milliseconds(),
	})
}

// vacuumWithProgress runs vacuum and emits progress events.
func (d *daemon) vacuumWithProgress(ctx context.Context, sender sseEventSender) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 1*time.Hour)
	defer cancel()

	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		logger.Warnf("vacuum: latest index id unavailable: %v", err)
		indexID = 0
	}

	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation: "vacuum",
		Phase:     "pre_checkpoint",
		Message:   "Running WAL checkpoint before vacuum...",
	}) {
		return
	}

	if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("vacuum: wal checkpoint (pre) failed: %v", err)
		if !sendSSEEvent(sender, "progress", WorkProgressEvent{
			Operation: "vacuum",
			Phase:     "pre_checkpoint",
			Message:   fmt.Sprintf("WAL checkpoint warning: %v", err),
		}) {
			return
		}
	}

	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation: "vacuum",
		Phase:     "vacuum",
		Message:   "Running VACUUM (this may take a while)...",
	}) {
		return
	}

	vs, err := storage.Vacuum(ctx, d.db)
	if err != nil {
		logger.Errorf("vacuum failed: %v", err)
		sendSSEError(sender, fmt.Sprintf("vacuum failed: %v", err))
		return
	}

	logger.Infof("Vacuum complete in %v", vs.Duration)

	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation: "vacuum",
		Phase:     "post_checkpoint",
		Message:   "Running WAL checkpoint after vacuum...",
	}) {
		return
	}

	if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("vacuum: wal checkpoint (post) failed: %v", err)
	}
	if err := storage.ReleaseSQLiteMemory(ctx, d.db); err != nil {
		logger.Warnf("Failed to release SQLite memory after vacuum: %v", err)
	}

	if indexID != 0 {
		if _, err := d.db.ExecContext(ctx, `UPDATE indexes SET vacuum_duration_ms = ? WHERE id = ?;`, vs.Duration.Milliseconds(), indexID); err != nil {
			logger.Warnf("vacuum: failed to persist duration: %v", err)
		}
	}

	duration := time.Since(start)
	sendSSEEvent(sender, "complete", WorkCompleteEvent{
		Status:     "complete",
		Operation:  "vacuum",
		DurationMs: duration.Milliseconds(),
	})
}

// pruneWithProgress prunes historical index data and emits status events.
func (d *daemon) pruneWithProgress(ctx context.Context, keepLatest, maxAgeDays int, sender sseEventSender) {
	ctx, cancel := context.WithTimeout(ctx, 1*time.Hour)
	defer cancel()

	maxAge := time.Duration(maxAgeDays) * 24 * time.Hour
	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation: "prune",
		Phase:     "prune",
		Message:   "Pruning old index records...",
	}) {
		return
	}

	stats, err := d.store.PruneOldIndexes(ctx, keepLatest, maxAge)
	if err != nil {
		logger.Errorf("prune failed: %v", err)
		sendSSEError(sender, fmt.Sprintf("prune failed: %v", err))
		return
	}

	logger.Infof("Prune complete in %v (deleted %d indexes, %d entries)",
		stats.Duration, stats.DeletedIndexes, stats.DeletedEntries)

	if !sendSSEEvent(sender, "progress", WorkProgressEvent{
		Operation: "prune",
		Phase:     "post_cleanup",
		Message:   "Running WAL checkpoint after prune...",
	}) {
		return
	}

	if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
		logger.Warnf("prune: wal checkpoint failed: %v", err)
	}
	if err := storage.ReleaseSQLiteMemory(ctx, d.db); err != nil {
		logger.Warnf("prune: failed to release SQLite memory: %v", err)
	}

	sendSSEEvent(sender, "complete", WorkCompleteEvent{
		Status:         "complete",
		Operation:      "prune",
		DurationMs:     stats.Duration.Milliseconds(),
		DeletedIndexes: stats.DeletedIndexes,
		DeletedEntries: stats.DeletedEntries,
	})
}
