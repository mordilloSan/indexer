package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mordilloSan/indexer/indexing"
	"github.com/mordilloSan/indexer/storage"
)

// DaemonConfig controls the long-running server.
type DaemonConfig struct {
	IndexName            string
	IndexPath            string
	IncludeHidden        bool
	IncludeNetworkMounts bool
	FreshIndex           bool
	KeepIndexes          int
	DBPath               string
	SocketPath           string
	ListenAddr           string
	Interval             time.Duration
}

type daemon struct {
	cfg             DaemonConfig
	db              *sql.DB
	store           *storage.Store
	servers         []*http.Server
	running         atomic.Bool
	workStreamMu    sync.RWMutex
	workStream      *workStreamBroadcaster
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
	switch cfg.SocketPath {
	case "-":
		cfg.SocketPath = ""
	case "":
		cfg.SocketPath = "/var/run/indexer.sock"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/var/run/indexer.db"
	}

	db, dbExisted, err := openDatabaseWithIntegrityCheck(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	slog.Info("DB connection pool opened", "db", cfg.DBPath)
	journalCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	journalMode, err := storage.GetJournalMode(journalCtx, db)
	if err != nil {
		slog.Warn("failed to determine database journal_mode", "err", err)
	} else {
		slog.Info("database journal_mode", "mode", strings.ToUpper(journalMode))
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
	slog.Info("shutting down daemon")

	// Gracefully shutdown HTTP servers
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, srv := range d.servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("server shutdown error", "err", err)
		}
	}

	// Close database connection
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			slog.Warn("database close error", "err", err)
		}
	}

	// Remove Unix socket only if we created it (not systemd-managed)
	if d.cfg.SocketPath != "" && !d.usedSystemdSock {
		if err := os.Remove(d.cfg.SocketPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to remove socket", "socket", d.cfg.SocketPath, "err", err)
		}
	}

	slog.Info("daemon shutdown complete")
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
			slog.Warn("failed to close listener after chmod error", "err", closeErr)
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
			slog.Warn("failed to close file after FileListener error", "err", closeErr)
		}
		return nil
	}

	// Clear environment to prevent child processes from inheriting
	if err := os.Unsetenv("LISTEN_PID"); err != nil {
		slog.Warn("failed to unset LISTEN_PID", "err", err)
	}
	if err := os.Unsetenv("LISTEN_FDS"); err != nil {
		slog.Warn("failed to unset LISTEN_FDS", "err", err)
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
		slog.Error("failed to check if database needs indexing", "err", err)
		return
	}

	if needsIndex {
		slog.Info("database needs initial indexing; spawning in background")
		go func() {
			if err := d.runIndexOnce(ctx); err != nil {
				slog.Error("initial index failed", "err", err)
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
				slog.Error("scheduled index failed", "err", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (d *daemon) startHTTP(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", serveOpenapi)
	mux.HandleFunc("/index", d.handleIndex)
	mux.HandleFunc("/reindex", d.handleReindex)
	mux.HandleFunc("/vacuum", d.handleVacuum)
	mux.HandleFunc("/prune", d.handlePrune)
	mux.HandleFunc("/status", d.handleStatus)
	mux.HandleFunc("/search", d.handleSearch)
	mux.HandleFunc("/dirsize", d.handleDirSize)
	mux.HandleFunc("/subfolders", d.handleSubfolders)
	mux.HandleFunc("/add", d.handleAdd)
	mux.HandleFunc("/delete", d.handleDelete)
	mux.HandleFunc("/entries", d.handleEntries)
	mux.HandleFunc("/config", d.handleConfig)

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
			slog.Info("API listening", "addr", "unix://"+d.cfg.SocketPath, "systemd_socket_activation", true)
		} else {
			slog.Info("API listening", "addr", "unix://"+d.cfg.SocketPath)
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
		slog.Info("API listening", "addr", "http://localhost"+d.cfg.ListenAddr)
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
			if err := srv.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("server shutdown error", "err", err)
			}
		}
		return nil
	case err := <-errCh:
		for _, srv := range d.servers {
			if shutdownErr := srv.Shutdown(context.Background()); shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
				slog.Warn("server shutdown error", "err", shutdownErr)
			}
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

func (d *daemon) setWorkStreamBroadcaster(b *workStreamBroadcaster) {
	d.workStreamMu.Lock()
	d.workStream = b
	d.workStreamMu.Unlock()
}

func (d *daemon) getWorkStreamBroadcaster() *workStreamBroadcaster {
	d.workStreamMu.RLock()
	b := d.workStream
	d.workStreamMu.RUnlock()
	return b
}

func (d *daemon) clearWorkStreamBroadcaster(b *workStreamBroadcaster) {
	d.workStreamMu.Lock()
	if d.workStream == b {
		d.workStream = nil
	}
	d.workStreamMu.Unlock()
}

// runIndexOnce performs an index by spawning the binary in index mode
func (d *daemon) runIndexOnce(ctx context.Context) error {
	if !d.tryLockIndex() {
		return fmt.Errorf("indexer already running")
	}
	defer d.unlockIndex()
	return d.runIndexSubprocess(ctx)
}

var indexerSpawnOverride func(*daemon, context.Context) error

// runIndexSubprocess spawns the current binary with --index-mode flag
// Uses systemd-run --scope to isolate memory accounting from the daemon's cgroup
func (d *daemon) runIndexSubprocess(ctx context.Context) error {
	if indexerSpawnOverride != nil {
		return indexerSpawnOverride(d, ctx)
	}
	// Build args for the index binary
	args := []string{
		"--index-mode",
		"--path", d.cfg.IndexPath,
		"--name", d.cfg.IndexName,
		"--db-path", d.cfg.DBPath,
	}
	if d.cfg.IncludeHidden {
		args = append(args, "--include-hidden")
	}
	if d.cfg.IncludeNetworkMounts {
		args = append(args, "--include-network-mounts")
	}
	args = append(args, "--fresh="+strconv.FormatBool(d.cfg.FreshIndex))
	if d.cfg.KeepIndexes > 0 {
		args = append(args, "--keep-indexes", strconv.Itoa(d.cfg.KeepIndexes))
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

	slog.Info("starting index process")

	if err := cmd.Run(); err != nil {
		systemdErr := err
		slog.Warn("systemd-run failed; falling back to direct execution", "err", systemdErr)

		fallbackCmd := exec.CommandContext(ctx, os.Args[0], args...)
		fallbackCmd.Stdout = os.Stdout
		fallbackCmd.Stderr = os.Stderr

		if err := fallbackCmd.Run(); err != nil {
			return fmt.Errorf("index subprocess failed after fallback: %w (systemd-run error: %v)", err, systemdErr)
		}
		slog.Info("index process completed successfully", "mode", "direct")
		return nil
	}

	slog.Info("index process completed successfully", "mode", "systemd-run")
	return nil
}

// RunIndexMode is called from main when -index-mode flag is set.
// It performs the index and returns when complete.
func RunIndexMode(indexName, indexPath string, includeHidden, includeNetworkMounts, fresh bool, dbPath string, keepIndexes int) error {
	slog.Info("running in index mode",
		"path", indexPath,
		"name", indexName,
		"db", dbPath,
		"fresh", fresh,
		"include_hidden", includeHidden,
		"include_network_mounts", includeNetworkMounts,
		"keep_indexes", keepIndexes,
	)

	db, _, err := openDatabaseWithIntegrityCheck(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Warn("failed to close database", "err", err)
		}
	}()

	ctx := context.Background()

	// In fresh mode, clear all existing data before indexing
	if fresh {
		if err := storage.ClearDatabase(ctx, db); err != nil {
			return fmt.Errorf("clear database: %w", err)
		}
		slog.Info("database cleared", "mode", "fresh")
	}

	if err := runIndex(ctx, db, indexName, indexPath, includeHidden, includeNetworkMounts); err != nil {
		return fmt.Errorf("run index: %w", err)
	}

	if keepIndexes > 0 {
		stats, err := storage.PruneOldIndexes(ctx, db, keepIndexes, 0)
		if err != nil {
			return fmt.Errorf("prune old indexes: %w", err)
		}
		slog.Info("automatic index retention applied",
			"keep_indexes", keepIndexes,
			"deleted_indexes", stats.DeletedIndexes,
			"deleted_entries", stats.DeletedEntries,
			"duration", stats.Duration,
		)
	}

	slog.Info("index complete, subprocess exiting")
	return nil
}

// runIndex performs the actual indexing work
func runIndex(ctx context.Context, db *sql.DB, indexName, indexPath string, includeHidden, includeNetworkMounts bool) error {
	index := indexing.Initialize(indexName, indexPath, indexPath, includeHidden, indexing.WithNetworkMounts(includeNetworkMounts))

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
	slog.Info("starting filesystem traversal")
	if err := index.StartIndexing(); err != nil {
		if closeErr := writer.Close(); closeErr != nil {
			slog.Warn("failed to close streaming writer after index error", "err", closeErr)
		}
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
		slog.Info("cleaned up deleted entries", "deleted", deleted)
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

	slog.Info("index complete",
		"duration", time.Since(start).Truncate(time.Millisecond),
		"dirs", index.NumDirs,
		"files", index.NumFiles,
		"size", index.GetTotalSize(),
	)

	if stats, err := storage.WALCheckpointTruncate(ctx, db); err != nil {
		slog.Warn("WAL checkpoint failed after index", "err", err)
	} else {
		slog.Info("WAL checkpoint complete after index", "duration", stats.Duration, "busy", stats.Busy, "log", stats.Log, "checkpointed", stats.Checkpointed)
	}
	if err := storage.ReleaseSQLiteMemory(ctx, db); err != nil {
		slog.Warn("failed to release SQLite memory after index", "err", err)
	}

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
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				slog.Warn("prepareIndexRecord rollback failed", "err", rollbackErr)
			}
		}
	}()

	// Get existing last_indexed timestamp if available
	var existingLastIndexed sql.NullInt64
	scanErr := tx.QueryRowContext(ctx, `SELECT last_indexed FROM indexes WHERE name = ?;`, indexName).Scan(&existingLastIndexed)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return 0, fmt.Errorf("query existing last_indexed: %w", scanErr)
	}
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

// openDatabaseWithIntegrityCheck opens a database and checks for corruption.
// If corrupted, it automatically removes and recreates the database.
// Returns the opened database connection and whether it existed before.
func openDatabaseWithIntegrityCheck(dbPath string) (*sql.DB, bool, error) {
	dbExisted := fileExists(dbPath)
	if dbExisted {
		slog.Info("database exists; checking integrity", "db", dbPath)
	} else {
		slog.Info("database not found; creating new", "db", dbPath)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, false, err
	}

	// Check for corruption if database existed
	if dbExisted {
		if err := checkDatabaseIntegrity(db); err != nil {
			slog.Warn("database corruption detected", "err", err)
			slog.Warn("closing corrupted database and recreating")
			if closeErr := db.Close(); closeErr != nil {
				slog.Warn("failed to close corrupted database", "err", closeErr)
			}
			// Remove database and associated WAL files
			if err := os.Remove(dbPath); err != nil {
				return nil, false, fmt.Errorf("failed to remove corrupted database: %w", err)
			}
			if err := os.Remove(dbPath + "-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("failed to remove WAL sidecar", "path", dbPath+"-wal", "err", err)
			}
			if err := os.Remove(dbPath + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
				slog.Warn("failed to remove SHM sidecar", "path", dbPath+"-shm", "err", err)
			}
			// Recreate fresh database
			db, err = storage.Open(dbPath)
			if err != nil {
				return nil, false, err
			}
			slog.Info("new database created", "db", dbPath)
			dbExisted = false // Treat as new database
		} else {
			slog.Info("database integrity check passed")
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
		slog.Info("latest index", "name", li.Name, "last_indexed", last, "dirs", li.NumDirs, "files", li.NumFiles)
	case sql.ErrNoRows:
		slog.Info("no prior index metadata found in database")
	default:
		slog.Warn("could not load latest index metadata", "err", err)
	}
}
