package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"indexer/indexing"
	"indexer/storage"
)

// DaemonConfig controls the long-running server.
type DaemonConfig struct {
	IndexName     string
	IndexPath     string
	IncludeHidden bool
	DBPath        string
	SocketPath    string
	ListenAddr    string
	Interval      time.Duration
}

type daemon struct {
	cfg     DaemonConfig
	db      *sql.DB
	servers []*http.Server
	running atomic.Bool
}

func NewDaemon(cfg DaemonConfig) (*daemon, error) {
	if cfg.IndexPath == "" {
		return nil, fmt.Errorf("index path is required")
	}
	if cfg.IndexName == "" {
		name := strings.ReplaceAll(cfg.IndexPath, "/", "_")
		if name == "" || name == "_" {
			name = "root"
		}
		cfg.IndexName = name
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = "/var/run/indexer.sock"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/var/run/indexer.db"
	}

	dbExisted := fileExists(cfg.DBPath)
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if dbExisted {
		logger.Infof("Database exists at %s; reusing existing indexes", cfg.DBPath)
		logLatestIndexStatus(db)
	} else {
		logger.Infof("Database not found; creating new at %s", cfg.DBPath)
	}

	return &daemon{
		cfg: cfg,
		db:  db,
	}, nil
}

func (d *daemon) Close() {
	logger.Infof("Shutting down daemon...")

	// Gracefully shutdown HTTP servers
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, srv := range d.servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warnf("Server shutdown error: %v", err)
		}
	}

	// Close database connection
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			logger.Warnf("Database close error: %v", err)
		}
	}

	// Remove Unix socket only if we created it (not systemd-managed)
	if d.cfg.SocketPath != "" && !isSystemdSocket() {
		if err := os.Remove(d.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
			logger.Warnf("Failed to remove socket: %v", err)
		}
	}

	logger.Infof("Daemon shutdown complete")
}

// getUnixListener returns a Unix socket listener, preferring systemd socket activation
func (d *daemon) getUnixListener() (net.Listener, bool, error) {
	// Try systemd socket activation first
	if l := systemdUnixListener(); l != nil {
		return l, true, nil
	}

	// Fallback: create socket manually
	if err := os.Remove(d.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("remove stale socket: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(d.cfg.SocketPath), 0o755); err != nil {
		return nil, false, fmt.Errorf("mkdir socket dir: %w", err)
	}

	l, err := net.Listen("unix", d.cfg.SocketPath)
	if err != nil {
		return nil, false, fmt.Errorf("listen on unix socket: %w", err)
	}
	if err := os.Chmod(d.cfg.SocketPath, 0o666); err != nil {
		l.Close()
		return nil, false, fmt.Errorf("chmod socket: %w", err)
	}

	return l, false, nil
}

// systemdUnixListener checks for systemd socket activation and returns the listener if available
func systemdUnixListener() net.Listener {
	// Systemd passes file descriptors via LISTEN_FDS and LISTEN_PID environment variables
	// FD 3 is the first passed file descriptor (after stdin=0, stdout=1, stderr=2)
	pid := os.Getenv("LISTEN_PID")
	fds := os.Getenv("LISTEN_FDS")

	if pid == "" || fds == "" {
		return nil
	}

	// Check if this process is the intended recipient
	if pid != strconv.Itoa(os.Getpid()) {
		return nil
	}

	// Check if exactly one FD was passed
	numFDs, err := strconv.Atoi(fds)
	if err != nil || numFDs != 1 {
		return nil
	}

	// File descriptor 3 is the first systemd-passed socket
	const systemdFD = 3
	file := os.NewFile(uintptr(systemdFD), "systemd-socket")
	if file == nil {
		return nil
	}

	// Convert file to listener
	l, err := net.FileListener(file)
	if err != nil {
		file.Close()
		return nil
	}

	// Clear environment to prevent child processes from inheriting
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")

	return l
}

// isSystemdSocket checks if we're using systemd socket activation
func isSystemdSocket() bool {
	pid := os.Getenv("LISTEN_PID")
	fds := os.Getenv("LISTEN_FDS")
	return pid != "" && fds != ""
}

// run starts the scheduler (if any) and blocks serving HTTP over a Unix socket.
func (d *daemon) Run(ctx context.Context) error {
	if d.cfg.Interval > 0 {
		go d.startScheduler(ctx)
	}
	return d.startHTTP(ctx)
}

func (d *daemon) startScheduler(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := d.runIndexOnce(ctx); err != nil {
				logger.Errorf("scheduled reindex failed: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *daemon) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", serveOpenapi)
	mux.HandleFunc("/reindex", d.handleReindex)
	mux.HandleFunc("/status", d.handleStatus)
	mux.HandleFunc("/search", d.handleSearch)
	mux.HandleFunc("/dirsize", d.handleDirSize)
	mux.HandleFunc("/add", d.handleAdd)
	mux.HandleFunc("/delete", d.handleDelete)
	mux.HandleFunc("/entries", d.handleEntries)

	errCh := make(chan error, 2)
	serverCount := 0

	// Unix socket listener - try systemd socket activation first
	if d.cfg.SocketPath != "" {
		l, isSystemd, err := d.getUnixListener()
		if err != nil {
			return err
		}

		srv := &http.Server{Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
		d.servers = append(d.servers, srv)
		serverCount++
		if isSystemd {
			logger.Infof("API listening on unix://%s (systemd socket activation)", d.cfg.SocketPath)
		} else {
			logger.Infof("API listening on unix://%s", d.cfg.SocketPath)
		}
		go func() {
			errCh <- srv.Serve(l)
		}()
	}

	// Optional TCP listener
	if d.cfg.ListenAddr != "" {
		tcpSrv := &http.Server{Addr: d.cfg.ListenAddr, Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
		d.servers = append(d.servers, tcpSrv)
		serverCount++
		logger.Infof("API listening on http://localhost%s", d.cfg.ListenAddr)
		go func() {
			errCh <- tcpSrv.ListenAndServe()
		}()
	}

	if serverCount == 0 {
		return fmt.Errorf("no listeners configured")
	}

	select {
	case <-ctx.Done():
		for _, srv := range d.servers {
			_ = srv.Shutdown(context.Background())
		}
		return nil
	case err := <-errCh:
		for _, srv := range d.servers {
			_ = srv.Shutdown(context.Background())
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (d *daemon) tryLockIndex() bool {
	return d.running.CompareAndSwap(false, true)
}

func (d *daemon) unlockIndex() {
	d.running.Store(false)
}

// runIndexOnce performs a full streaming reindex using the shared DB connection.
func (d *daemon) runIndexOnce(ctx context.Context) error {
	if !d.tryLockIndex() {
		return fmt.Errorf("indexer already running")
	}
	defer d.unlockIndex()
	return d.runIndex(ctx)
}

// runIndex contains the indexing workflow and assumes the caller holds the lock.
func (d *daemon) runIndex(ctx context.Context) error {
	index := indexing.Initialize(d.cfg.IndexName, d.cfg.IndexPath, d.cfg.IndexPath, d.cfg.IncludeHidden)

	start := time.Now()

	indexID, err := d.prepareIndexRecord(ctx)
	if err != nil {
		return err
	}

	writer := storage.NewStreamingWriter(ctx, d.db, indexID, 1000)
	index.EnableStreaming(writer)

	if err := index.StartIndexing(); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("streaming writer: %w", err)
	}

	// Cleanup deleted entries (files that were not seen during this scan)
	scanTime := writer.ScanTime()
	deleted, err := storage.CleanupDeletedEntries(ctx, d.db, indexID, scanTime)
	if err != nil {
		return fmt.Errorf("cleanup deleted entries: %w", err)
	}
	if deleted > 0 {
		logger.Infof("Cleaned up %d deleted entries", deleted)
	}

	if _, err := d.db.ExecContext(ctx, `
		UPDATE indexes SET
			num_dirs = ?,
			num_files = ?,
			total_size = ?,
			disk_used = ?,
			disk_total = ?,
			last_indexed = ?
		WHERE id = ?;
	`,
		index.NumDirs,
		index.NumFiles,
		int64(index.GetTotalSize()),
		int64(index.DiskUsed),
		int64(index.DiskTotal),
		time.Now().UTC().Unix(),
		indexID,
	); err != nil {
		index.Cleanup()
		runtime.GC()
		debug.FreeOSMemory()
		runtime.GC()
		return fmt.Errorf("update index metadata: %w", err)
	}
	logger.Infof("Index run complete in %v (dirs=%d files=%d)", time.Since(start).Truncate(time.Millisecond), index.NumDirs, index.NumFiles)

	// Free memory from Index's internal maps before GC
	index.Cleanup()

	// Release SQLite internal caches
	if err := storage.ReleaseSQLiteMemory(ctx, d.db); err != nil {
		logger.Warnf("Failed to release SQLite memory: %v", err)
	}

	// Aggressively reclaim memory from indexing
	runtime.GC()         // Run garbage collection
	debug.FreeOSMemory() // Force return memory to OS immediately
	runtime.GC()         // Run GC again after freeing

	return nil
}

func (d *daemon) prepareIndexRecord(ctx context.Context) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var existingLastIndexed sql.NullInt64
	_ = tx.QueryRowContext(ctx, `SELECT last_indexed FROM indexes WHERE name = ?;`, d.cfg.IndexName).Scan(&existingLastIndexed)
	lastIndexed := int64(0)
	if existingLastIndexed.Valid {
		lastIndexed = existingLastIndexed.Int64
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO indexes (
			name, root_path, source, include_hidden,
			num_dirs, num_files, total_size, disk_used,
			disk_total, last_indexed,
			index_duration_ms, export_duration_ms, vacuum_duration_ms
		) VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, ?, 0, 0, 0)
		ON CONFLICT(name) DO UPDATE SET
			root_path=excluded.root_path,
			source=excluded.source,
			include_hidden=excluded.include_hidden;
	`,
		d.cfg.IndexName,
		d.cfg.IndexPath,
		d.cfg.IndexPath,
		indexing.BoolToInt(d.cfg.IncludeHidden),
		lastIndexed,
	)
	if err != nil {
		return 0, err
	}

	var indexID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM indexes WHERE name = ?;`, d.cfg.IndexName).Scan(&indexID); err != nil {
		return 0, err
	}

	err = tx.Commit()
	return indexID, err
}

func (d *daemon) latestIndexID(ctx context.Context) (int64, error) {
	var id int64
	err := d.db.QueryRowContext(ctx, `SELECT id FROM indexes ORDER BY last_indexed DESC LIMIT 1`).Scan(&id)
	return id, err
}

func (d *daemon) handleReindex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !d.tryLockIndex() {
		http.Error(w, "indexer already running", http.StatusConflict)
		return
	}
	go func() {
		defer d.unlockIndex()
		if err := d.runIndex(context.Background()); err != nil {
			logger.Errorf("manual reindex failed: %v", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"running"}`))
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var stats struct {
		Status      string `json:"status"`
		NumDirs     int64  `json:"num_dirs"`
		NumFiles    int64  `json:"num_files"`
		TotalSize   int64  `json:"total_size"`
		LastIndexed string `json:"last_indexed"`
	}
	if d.running.Load() {
		stats.Status = "running"
	} else {
		stats.Status = "idle"
	}

	// Get database statistics from the most recent index
	var numDirs, numFiles, totalSize sql.NullInt64
	var lastIndexed sql.NullInt64
	err := d.db.QueryRowContext(ctx, `
		SELECT num_dirs, num_files, total_size, last_indexed
		FROM indexes
		ORDER BY last_indexed DESC
		LIMIT 1
	`).Scan(&numDirs, &numFiles, &totalSize, &lastIndexed)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no data yet; keep defaults
	case err != nil:
		http.Error(w, fmt.Sprintf("error loading status: %v", err), http.StatusInternalServerError)
		return
	default:
		if numDirs.Valid {
			stats.NumDirs = numDirs.Int64
		}
		if numFiles.Valid {
			stats.NumFiles = numFiles.Int64
		}
		if totalSize.Valid {
			stats.TotalSize = totalSize.Int64
		}
		if lastIndexed.Valid && lastIndexed.Int64 > 0 {
			stats.LastIndexed = time.Unix(lastIndexed.Int64, 0).UTC().Format(time.RFC3339)
		}
	}

	writeJSON(w, stats)
}

func (d *daemon) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	store := storage.NewStoreWithDB(d.db)
	results, err := store.SearchEntries(q, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleEntries(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	recursive := r.URL.Query().Get("recursive") == "true"
	limit := 200
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 {
			limit = v
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	store := storage.NewStoreWithDB(d.db)
	results, err := store.QueryPath(path, recursive, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleDirSize(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	store := storage.NewStoreWithDB(d.db)
	total, err := store.DirSize(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{
		"path":  path,
		"size":  total,
		"bytes": total,
	})
}

func (d *daemon) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Path    string `json:"path"`
		AbsPath string `json:"absPath"`
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		IsDir   bool   `json:"isDir"`
		Type    string `json:"type"`
		Hidden  bool   `json:"hidden"`
		ModUnix int64  `json:"modUnix"`
		Inode   uint64 `json:"inode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if payload.Path == "" || payload.Name == "" {
		http.Error(w, "path and name are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	indexID, err := d.latestIndexID(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("no index present: %v", err), http.StatusBadRequest)
		return
	}

	relPath := indexing.NormalizeIndexPath(payload.Path)
	absPath := payload.AbsPath
	if absPath == "" {
		absPath = payload.Path
	}
	modUnix := payload.ModUnix
	if modUnix == 0 {
		modUnix = time.Now().UTC().Unix()
	}
	entryType := payload.Type
	if entryType == "" {
		if payload.IsDir {
			entryType = "directory"
		} else {
			entryType = "file"
		}
	}

	entry := indexing.IndexEntry{
		RelativePath: relPath,
		AbsolutePath: absPath,
		Name:         payload.Name,
		Size:         payload.Size,
		ModTime:      time.Unix(modUnix, 0),
		Type:         entryType,
		Hidden:       payload.Hidden,
		IsDir:        payload.IsDir,
		Inode:        payload.Inode,
	}

	if err := storage.UpsertEntryWithSizeUpdate(ctx, d.db, indexID, entry); err != nil {
		http.Error(w, fmt.Sprintf("upsert failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (d *daemon) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "use DELETE", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	indexID, err := d.latestIndexID(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("no index present: %v", err), http.StatusBadRequest)
		return
	}

	relPath := indexing.NormalizeIndexPath(path)
	if err := storage.DeleteEntryWithSizeUpdate(ctx, d.db, indexID, relPath); err != nil {
		http.Error(w, fmt.Sprintf("delete failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Minimal OpenAPI spec served at /openapi.json
func serveOpenapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(openapiSpec))
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func logLatestIndexStatus(db *sql.DB) {
	var name sql.NullString
	var lastIndexed sql.NullInt64
	var numDirs, numFiles sql.NullInt64
	err := db.QueryRow(`
		SELECT name, last_indexed, num_dirs, num_files
		FROM indexes
		ORDER BY last_indexed DESC
		LIMIT 1;
	`).Scan(&name, &lastIndexed, &numDirs, &numFiles)

	switch err {
	case nil:
		last := "unknown"
		if lastIndexed.Valid && lastIndexed.Int64 > 0 {
			last = time.Unix(lastIndexed.Int64, 0).UTC().Format(time.RFC3339)
		}
		logger.Infof("Latest index: name=%s last_indexed=%s dirs=%d files=%d",
			nullStringOr(name, "<none>"),
			last,
			nullInt64Or(numDirs, 0),
			nullInt64Or(numFiles, 0),
		)
	case sql.ErrNoRows:
		logger.Infof("No prior index metadata found in database")
	default:
		logger.Warnf("Could not load latest index metadata: %v", err)
	}
}

func nullStringOr(v sql.NullString, def string) string {
	if v.Valid {
		return v.String
	}
	return def
}

func nullInt64Or(v sql.NullInt64, def int64) int64 {
	if v.Valid {
		return v.Int64
	}
	return def
}

const openapiSpec = `{
  "openapi": "3.0.0",
  "info": { "title": "Indexer API", "version": "1.0.0" },
  "paths": {
    "/reindex": { "post": { "summary": "Trigger reindex", "responses": { "202": {"description": "Started"}, "409": {"description": "Already running"} } } },
    "/status": { "get": { "summary": "Get status", "responses": { "200": {"description": "Status"} } } },
    "/search": { "get": { "summary": "Search entries", "parameters": [{ "in": "query", "name": "q", "schema": {"type": "string"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Results"} } } },
    "/dirsize": { "get": { "summary": "Directory size", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "Size"} } } },
    "/entries": { "get": { "summary": "List entries", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }, { "in": "query", "name": "recursive", "schema": {"type": "boolean"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }, { "in": "query", "name": "offset", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Entries"} } } },
    "/add": { "post": { "summary": "Upsert entry", "responses": { "200": {"description": "OK"} } } },
    "/delete": { "delete": { "summary": "Delete entry", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "OK"} } } }
  }
}`
