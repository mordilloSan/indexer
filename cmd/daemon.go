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
	"os/exec"
	"path/filepath"
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
	cfg             DaemonConfig
	db              *sql.DB
	store           *storage.Store
	servers         []*http.Server
	running         atomic.Bool
	usedSystemdSock bool
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
	if cfg.SocketPath == "-" {
		cfg.SocketPath = ""
	} else if cfg.SocketPath == "" {
		cfg.SocketPath = "/var/run/indexer.sock"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/var/run/indexer.db"
	}

	db, dbExisted, err := openDatabaseWithIntegrityCheck(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	logger.Infof("DB connection pool opened: %s", cfg.DBPath)
	journalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	journalMode, err := storage.GetJournalMode(journalCtx, db)
	if err != nil {
		logger.Warnf("Failed to determine database journal_mode: %v", err)
	} else {
		logger.Infof("Database journal_mode: %s", strings.ToUpper(journalMode))
	}

	if dbExisted {
		logLatestIndexStatus(db)
	}

	return &daemon{
		cfg:   cfg,
		db:    db,
		store: storage.NewStoreWithDB(db, cfg.DBPath),
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
	if d.cfg.SocketPath != "" && !d.usedSystemdSock {
		if err := os.Remove(d.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
			logger.Warnf("Failed to remove socket: %v", err)
		}
	}

	logger.Infof("Daemon shutdown complete")
}

// getUnixListener returns a Unix socket listener, preferring systemd socket activation
func (d *daemon) getUnixListener() (net.Listener, error) {
	// Try systemd socket activation first
	if l := systemdUnixListener(); l != nil {
		d.usedSystemdSock = true
		return l, nil
	}

	// Fallback: create socket manually
	d.usedSystemdSock = false
	if err := os.Remove(d.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(d.cfg.SocketPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}

	l, err := net.Listen("unix", d.cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket: %w", err)
	}
	if err := os.Chmod(d.cfg.SocketPath, 0o666); err != nil {
		if closeErr := l.Close(); closeErr != nil {
			logger.Warnf("Failed to close listener after chmod error: %v", closeErr)
		}
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return l, nil
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
		if closeErr := file.Close(); closeErr != nil {
			logger.Warnf("Failed to close file after FileListener error: %v", closeErr)
		}
		return nil
	}

	// Clear environment to prevent child processes from inheriting
	if err := os.Unsetenv("LISTEN_PID"); err != nil {
		logger.Warnf("Failed to unset LISTEN_PID: %v", err)
	}
	if err := os.Unsetenv("LISTEN_FDS"); err != nil {
		logger.Warnf("Failed to unset LISTEN_FDS: %v", err)
	}

	return l
}

// Run starts the scheduler (if any) and HTTP server, blocks until context is cancelled
func (d *daemon) Run(ctx context.Context) error {
	// Start periodic indexing scheduler if configured
	if d.cfg.Interval > 0 {
		go d.startScheduler(ctx)
	}
	return d.startHTTP(ctx)
}

// SpawnInitialIndex checks if database needs initial indexing and triggers it if needed
func (d *daemon) SpawnInitialIndex(ctx context.Context) {
	needsIndex, err := d.needsInitialIndex()
	if err != nil {
		logger.Errorf("Failed to check if database needs indexing: %v", err)
		return
	}

	if needsIndex {
		logger.Infof("Database needs initial indexing; spawning in background")
		go func() {
			if err := d.runIndexOnce(ctx); err != nil {
				logger.Errorf("Initial index failed: %v", err)
			}
		}()
	}
}

// needsInitialIndex checks if database is empty OR index has never been indexed
func (d *daemon) needsInitialIndex() (bool, error) {
	// Check if there are any indexes
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM indexes`).Scan(&count)
	if err != nil {
		return false, err
	}

	// No indexes at all - needs indexing
	if count == 0 {
		return true, nil
	}

	// Check if the index has ever been indexed (last_indexed is NULL or 0)
	var lastIndexed sql.NullInt64
	err = d.db.QueryRow(`SELECT last_indexed FROM indexes WHERE name = ? LIMIT 1`, d.cfg.IndexName).Scan(&lastIndexed)
	if err != nil {
		if err == sql.ErrNoRows {
			// Index with this name doesn't exist - needs indexing
			return true, nil
		}
		return false, err
	}

	// If last_indexed is NULL or 0, needs indexing
	return !lastIndexed.Valid || lastIndexed.Int64 == 0, nil
}

func (d *daemon) startScheduler(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := d.runIndexOnce(ctx); err != nil {
				logger.Errorf("scheduled index failed: %v", err)
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
		l, err := d.getUnixListener()
		if err != nil {
			return err
		}

		srv := &http.Server{Handler: mux, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
		d.servers = append(d.servers, srv)
		serverCount++
		if d.usedSystemdSock {
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

	// Spawn initial index if database is empty (after API is ready)
	d.SpawnInitialIndex(ctx)

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

// runIndexOnce performs an index by spawning the binary in index mode
func (d *daemon) runIndexOnce(ctx context.Context) error {
	if !d.tryLockIndex() {
		return fmt.Errorf("indexer already running")
	}
	defer d.unlockIndex()
	return d.runIndexSubprocess(ctx)
}

// runIndexSubprocess spawns the current binary with --reindex-mode flag
// Uses systemd-run --scope to isolate memory accounting from the daemon's cgroup
func (d *daemon) runIndexSubprocess(ctx context.Context) error {
	// Build args for the index binary
	args := []string{
		"--reindex-mode",
		"--path", d.cfg.IndexPath,
		"--name", d.cfg.IndexName,
		"--db-path", d.cfg.DBPath,
	}
	if d.cfg.IncludeHidden {
		args = append(args, "--include-hidden")
	}

	// Use systemd-run to spawn in a separate cgroup/scope
	// This ensures proper memory accounting - the daemon's cgroup won't include subprocess memory
	unitName := fmt.Sprintf("indexer-index-%d", time.Now().Unix())
	systemdArgs := []string{
		"--scope",            // Run as a transient scope (not a full service)
		"--unit=" + unitName, // Give it a unique name
		os.Args[0],           // The binary to run (still secure - uses os.Args[0])
	}
	systemdArgs = append(systemdArgs, args...) // Add index-mode args

	cmd := exec.CommandContext(ctx, "systemd-run", systemdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Infof("Starting index process")

	if err := cmd.Run(); err != nil {
		systemdErr := err
		logger.Warnf("systemd-run failed (%v); falling back to direct execution", systemdErr)

		fallbackCmd := exec.CommandContext(ctx, os.Args[0], args...)
		fallbackCmd.Stdout = os.Stdout
		fallbackCmd.Stderr = os.Stderr

		if err := fallbackCmd.Run(); err != nil {
			return fmt.Errorf("index subprocess failed after fallback: %w (systemd-run error: %v)", err, systemdErr)
		}
		logger.Infof("Index process completed successfully (direct execution fallback)")
		return nil
	}

	logger.Infof("Index process completed successfully")
	return nil
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
		if err := d.runIndexSubprocess(context.Background()); err != nil {
			logger.Errorf("manual index failed: %v", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"running"}`))
}

// RunIndexMode is called from main when --reindex-mode flag is set
// It performs the index and exits (releasing all memory including leaks)
func RunIndexMode(indexName, indexPath string, includeHidden bool, dbPath string, verbose bool) {
	logger.Infof("Running in index mode: path=%s name=%s db=%s", indexPath, indexName, dbPath)

	db, _, err := openDatabaseWithIntegrityCheck(dbPath)
	if err != nil {
		logger.Fatalf("Failed to open database: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Warnf("Failed to close database: %v", err)
		}
	}()

	// Run index
	ctx := context.Background()
	if err := runIndex(ctx, db, indexName, indexPath, includeHidden); err != nil {
		logger.Fatalf("Index failed: %v", err)
	}

	logger.Infof("Index complete, subprocess exiting")
}

// runIndex performs the actual indexing work
func runIndex(ctx context.Context, db *sql.DB, indexName, indexPath string, includeHidden bool) error {
	index := indexing.Initialize(indexName, indexPath, indexPath, includeHidden)

	start := time.Now()

	// Prepare index record in database
	indexID, err := prepareIndexRecord(ctx, db, indexName, indexPath, includeHidden)
	if err != nil {
		return fmt.Errorf("prepare index record: %w", err)
	}

	// Create streaming writer with batch size of 1000
	writer := storage.NewStreamingWriter(ctx, db, indexID, 1000)
	index.EnableStreaming(writer)

	// Start filesystem traversal
	logger.Infof("Starting filesystem traversal...")
	if err := index.StartIndexing(); err != nil {
		_ = writer.Close()
		return fmt.Errorf("indexing failed: %w", err)
	}

	// Flush remaining batches
	if err := writer.Close(); err != nil {
		return fmt.Errorf("streaming writer close: %w", err)
	}

	// Cleanup deleted entries (files that were not seen during this scan)
	scanTime := writer.ScanTime()
	deleted, err := storage.CleanupDeletedEntries(ctx, db, indexID, scanTime)
	if err != nil {
		return fmt.Errorf("cleanup deleted entries: %w", err)
	}
	if deleted > 0 {
		logger.Infof("Cleaned up %d deleted entries", deleted)
	}

	// Update index metadata
	if _, err := db.ExecContext(ctx, `
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
		return fmt.Errorf("update index metadata: %w", err)
	}

	logger.Infof("Index complete in %v (dirs=%d files=%d size=%d)",
		time.Since(start).Truncate(time.Millisecond),
		index.NumDirs,
		index.NumFiles,
		index.GetTotalSize(),
	)

	return nil
}

// prepareIndexRecord creates or updates the index record in the database
func prepareIndexRecord(ctx context.Context, db *sql.DB, indexName, indexPath string, includeHidden bool) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Get existing last_indexed timestamp if available
	var existingLastIndexed sql.NullInt64
	_ = tx.QueryRowContext(ctx, `SELECT last_indexed FROM indexes WHERE name = ?;`, indexName).Scan(&existingLastIndexed)
	lastIndexed := int64(0)
	if existingLastIndexed.Valid {
		lastIndexed = existingLastIndexed.Int64
	}

	// Insert or update index record
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
		indexName,
		indexPath,
		indexPath,
		indexing.BoolToInt(includeHidden),
		lastIndexed,
	)
	if err != nil {
		return 0, fmt.Errorf("insert/update index: %w", err)
	}

	// Get the index ID
	var indexID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM indexes WHERE name = ?;`, indexName).Scan(&indexID); err != nil {
		return 0, fmt.Errorf("get index id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return indexID, nil
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	running := d.running.Load()

	// Response shape (backwards-compatible + extra stats)
	var resp struct {
		Status       string `json:"status"`
		NumDirs      int64  `json:"num_dirs"`
		NumFiles     int64  `json:"num_files"`
		TotalSize    int64  `json:"total_size"`
		LastIndexed  string `json:"last_indexed"`
		TotalIndexes int    `json:"total_indexes"`
		TotalEntries int64  `json:"total_entries"`
		DatabaseSize int64  `json:"database_size"`
		Warning      string `json:"warning,omitempty"`
	}

	if running {
		resp.Status = "running"
	} else {
		resp.Status = "idle"
	}

	addWarning := func(msg string) {
		if resp.Warning == "" {
			resp.Warning = msg
		} else {
			resp.Warning += "; " + msg
		}
	}

	// 1) Per-latest-index stats (dirs/files/size/last_indexed)
	li, err := loadLatestIndex(ctx, d.db)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No prior index: leave NumDirs/NumFiles/TotalSize/LastIndexed as zero values
	case err != nil:
		if running {
			addWarning(fmt.Sprintf("latest index unavailable: %v", err))
			logger.Warnf("Status: latest index unavailable while indexing: %v", err)
		} else {
			http.Error(w, fmt.Sprintf("error loading status: %v", err), http.StatusInternalServerError)
			return
		}
	default:
		resp.NumDirs = li.NumDirs
		resp.NumFiles = li.NumFiles
		resp.TotalSize = li.TotalSize
		if li.LastIndexed.Valid && li.LastIndexed.Int64 > 0 {
			resp.LastIndexed = time.Unix(li.LastIndexed.Int64, 0).UTC().Format(time.RFC3339)
		}
	}

	// 2) Global stats (uses Store with correct dbPath for db size)
	stats, err := d.store.GetStats(ctx)
	if err != nil {
		if running {
			addWarning(fmt.Sprintf("stats unavailable: %v", err))
			logger.Warnf("Status: global stats unavailable while indexing: %v", err)
		} else {
			http.Error(w, fmt.Sprintf("error loading stats: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if stats != nil {
		resp.TotalIndexes = stats.TotalIndexes
		resp.TotalEntries = stats.TotalEntries
		resp.DatabaseSize = stats.DatabaseSize
	}

	writeJSON(w, resp)
}

func (d *daemon) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := queryInt(r.URL.Query().Get("limit"), 100, 1)
	results, err := d.store.SearchEntries(ctx, q, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := queryPathOrRoot(r.URL.Query().Get("path"))
	recursive := r.URL.Query().Get("recursive") == "true"
	limit := queryInt(r.URL.Query().Get("limit"), 200, 1)
	offset := queryInt(r.URL.Query().Get("offset"), 0, 0)

	results, err := d.store.QueryPath(ctx, path, recursive, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleDirSize(w http.ResponseWriter, r *http.Request) {
	path := queryPathOrRoot(r.URL.Query().Get("path"))
	total, err := d.store.DirSize(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{
		"path":  path,
		"size":  total,
		"bytes": total, // backwards compatibility
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
	indexID, err := d.store.LatestIndexID(ctx)
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

	if err := d.store.UpsertEntryWithSizeUpdate(ctx, indexID, entry); err != nil {
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
	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("no index present: %v", err), http.StatusBadRequest)
		return
	}

	relPath := indexing.NormalizeIndexPath(path)
	if err := d.store.DeleteEntryWithSizeUpdate(ctx, indexID, relPath); err != nil {
		http.Error(w, fmt.Sprintf("delete failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// queryInt parses an integer query parameter with default and minimum value
func queryInt(q string, def int, min int) int {
	if q == "" {
		return def
	}
	v, err := strconv.Atoi(q)
	if err != nil || v < min {
		return def
	}
	return v
}

// queryPathOrRoot returns the path query parameter or "/" if empty
func queryPathOrRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// Minimal OpenAPI spec served at /openapi.json
func serveOpenapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(openapiSpec))
}

// openDatabaseWithIntegrityCheck opens a database and checks for corruption.
// If corrupted, it automatically removes and recreates the database.
// Returns the opened database connection and whether it existed before.
func openDatabaseWithIntegrityCheck(dbPath string) (*sql.DB, bool, error) {
	dbExisted := fileExists(dbPath)
	if dbExisted {
		logger.Infof("Database exists at %s; checking integrity", dbPath)
	} else {
		logger.Infof("Database not found; creating new at %s", dbPath)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, false, err
	}

	// Check for corruption if database existed
	if dbExisted {
		if err := checkDatabaseIntegrity(db); err != nil {
			logger.Warnf("Database corruption detected: %v", err)
			logger.Warnf("Closing corrupted database and recreating")
			if closeErr := db.Close(); closeErr != nil {
				logger.Warnf("Failed to close corrupted database: %v", closeErr)
			}
			// Remove database and associated WAL files
			if err := os.Remove(dbPath); err != nil {
				return nil, false, fmt.Errorf("failed to remove corrupted database: %w", err)
			}
			_ = os.Remove(dbPath + "-wal")
			_ = os.Remove(dbPath + "-shm")
			// Recreate fresh database
			db, err = storage.Open(dbPath)
			if err != nil {
				return nil, false, err
			}
			logger.Infof("New database created at %s", dbPath)
			dbExisted = false // Treat as new database
		} else {
			logger.Infof("Database integrity check passed")
		}
	}

	return db, dbExisted, nil
}

// checkDatabaseIntegrity runs SQLite's integrity_check to detect corruption
func checkDatabaseIntegrity(db *sql.DB) error {
	var result string
	err := db.QueryRow("PRAGMA integrity_check;").Scan(&result)
	if err != nil {
		return fmt.Errorf("integrity check query failed: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity check failed: %s", result)
	}
	return nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// LatestIndex represents the most recent index metadata
type LatestIndex struct {
	Name        string
	NumDirs     int64
	NumFiles    int64
	TotalSize   int64
	LastIndexed sql.NullInt64
}

// loadLatestIndex fetches the most recent index metadata from the database
func loadLatestIndex(ctx context.Context, db *sql.DB) (*LatestIndex, error) {
	var li LatestIndex
	var name sql.NullString
	var numDirs, numFiles, totalSize sql.NullInt64

	err := db.QueryRowContext(ctx, `
		SELECT name, num_dirs, num_files, total_size, last_indexed
		FROM indexes
		ORDER BY last_indexed DESC
		LIMIT 1
	`).Scan(&name, &numDirs, &numFiles, &totalSize, &li.LastIndexed)
	if err != nil {
		return nil, err
	}

	if name.Valid {
		li.Name = name.String
	}
	if numDirs.Valid {
		li.NumDirs = numDirs.Int64
	}
	if numFiles.Valid {
		li.NumFiles = numFiles.Int64
	}
	if totalSize.Valid {
		li.TotalSize = totalSize.Int64
	}

	return &li, nil
}

func logLatestIndexStatus(db *sql.DB) {
	ctx := context.Background()
	li, err := loadLatestIndex(ctx, db)

	switch err {
	case nil:
		last := "unknown"
		if li.LastIndexed.Valid && li.LastIndexed.Int64 > 0 {
			last = time.Unix(li.LastIndexed.Int64, 0).UTC().Format(time.RFC3339)
		}
		logger.Infof("Latest index: name=%s last_indexed=%s dirs=%d files=%d",
			li.Name,
			last,
			li.NumDirs,
			li.NumFiles,
		)
	case sql.ErrNoRows:
		logger.Infof("No prior index metadata found in database")
	default:
		logger.Warnf("Could not load latest index metadata: %v", err)
	}
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
