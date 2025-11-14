package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/mordilloSan/go_logger/logger"

	"indexer/indexing"
	"indexer/storage"
)

func main() {
	if len(os.Args) < 2 {
		printRootUsage()
		os.Exit(0)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "index":
		err = runIndexCommand(args)
	case "daemon":
		err = runDaemonCommand()
	case "refresh":
		err = runRefreshCommand(args)
	case "search":
		err = runSearchCommand(args)
	case "serve":
		err = runServeCommand(args)
	case "stats":
		err = runStatsCommand(args)
	case "size":
		err = runSizeCommand(args)
	case "socket":
		err = runSocketCommand(args)
	case "watch":
		err = runWatchCommand(args)
	case "-h", "--help", "help":
		printRootUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		printRootUsage()
		return
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printRootUsage() {
	fmt.Println("Usage: indexer <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  index    Run a full index and persist to SQLite")
	fmt.Println("  daemon   Run as a long-lived indexing daemon")
	fmt.Println("  refresh  Refresh specific paths using an existing snapshot")
	fmt.Println("  search   Search using an existing snapshot (no new scan)")
	fmt.Println("  serve    Run a simple HTTP API backed by SQLite")
	fmt.Println("  stats    Show aggregate directory statistics from SQLite")
	fmt.Println("  size     Show only the total size of a directory from SQLite")
	fmt.Println("  socket   Run a Unix socket server for fast local queries")
	fmt.Println("  watch    Watch paths and trigger event-driven refreshes")
	fmt.Println()
	fmt.Println("Run 'indexer <command> -h' for command-specific flags.")
}

func initLogger(verbose bool) {
	mode := os.Getenv("INDEXER_ENV")
	if mode == "" {
		mode = "production"
	}
	logger.Init(mode, verbose)
}

func runIndexCommand(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexPath := fs.String("path", "", "Path to index (required, e.g., /, /home, /var)")
	indexName := fs.String("name", "", "Name for this index (optional, defaults to path)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files and directories (starting with .)")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")
	noRateLimit := fs.Bool("no-rate-limit", false, "Disable rate limiting for this run")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	if *indexPath == "" {
		fs.Usage()
		return fmt.Errorf("-path is required")
	}

	name := deriveIndexName(*indexName, *indexPath)
	dbPath := deriveDBPath(*dbPathFlag)

	return indexOnce(*indexPath, name, dbPath, *includeHidden, *noRateLimit)
}

func runRefreshCommand(args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexPath := fs.String("path", "", "Root path for this index (must match the original -path)")
	indexName := fs.String("name", "", "Name of the existing index (required)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	refreshRecursive := fs.Bool("refresh-recursive", false, "Recursively refresh directories supplied via -refresh-path")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	var refreshPaths stringSliceFlag
	fs.Var(&refreshPaths, "refresh-path", "Absolute file or directory to refresh without a full scan (repeatable, required)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	if *indexName == "" {
		fs.Usage()
		return fmt.Errorf("-name is required")
	}
	if len(refreshPaths) == 0 {
		fs.Usage()
		return fmt.Errorf("at least one -refresh-path is required")
	}

	dbPath := deriveDBPath(*dbPathFlag)
	rootPath := *indexPath
	if rootPath == "" {
		// Use the index name as a fallback root path; Search/refresh relies on snapshot content more than this value.
		rootPath = "/"
	}

	idx := indexing.Initialize(*indexName, rootPath, rootPath, false)

	return runRefreshMode(idx, *indexName, dbPath, refreshPaths, *refreshRecursive)
}

func runRefreshMode(idx *indexing.Index, indexName, dbPath string, refreshPaths []string, recursive bool) error {
	if dbPath == "" {
		return fmt.Errorf("refresh mode requires -db-path or INDEXER_DB_PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := storage.LoadSnapshotIntoIndex(ctx, dbPath, indexName, idx); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	start := time.Now()
	var refreshed int
	for _, raw := range refreshPaths {
		absPath, err := normalizeTargetPath(idx.Path, raw)
		if err != nil {
			return err
		}
		if err := idx.RefreshAbsolutePath(absPath, recursive); err != nil {
			return fmt.Errorf("refresh %s: %w", absPath, err)
		}
		refreshed++
	}

	saveCtx, cancelSave := context.WithTimeout(context.Background(), time.Minute)
	defer cancelSave()
	metrics := &storage.PersistMetrics{IndexDuration: time.Since(start)}

	if err := storage.SaveIndexToFile(saveCtx, dbPath, idx, metrics); err != nil {
		return fmt.Errorf("persist refresh: %w", err)
	}

	logger.Infof("Refreshed %d path(s); export=%v vacuum=%v", refreshed, metrics.ExportDuration, metrics.VacuumDuration)
	return nil
}

func indexOnce(indexPath, name, dbPath string, includeHidden, noRateLimit bool) error {
	logger.Infof("Initializing indexer for path: %s", indexPath)
	idx := indexing.Initialize(name, indexPath, indexPath, includeHidden)

	if !noRateLimit {
		rateLimiter := newRateLimiter(dbPath)
		if err := rateLimiter.Enforce(); err != nil {
			return err
		}
	}

	logger.Infof("Starting indexing...")
	startTime := time.Now()
	if err := idx.StartIndexing(); err != nil {
		return fmt.Errorf("indexing: %w", err)
	}
	duration := time.Since(startTime)

	logger.Infof("Indexing completed successfully!")
	logger.Infof("Total directories: %d", idx.NumDirs)
	logger.Infof("Total files: %d", idx.NumFiles)
	logger.Infof("Total size: %d bytes (%.2f GB)", idx.GetTotalSize(), float64(idx.GetTotalSize())/(1024*1024*1024))
	logger.Infof("Indexing duration: %v", duration)

	saveCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	metrics := &storage.PersistMetrics{IndexDuration: duration}
	if err := storage.SaveIndexToFile(saveCtx, dbPath, idx, metrics); err != nil {
		return fmt.Errorf("persist index: %w", err)
	}

	logger.Infof(
		"Saved index '%s' to database (export: %v, vacuum: %v)",
		idx.Name,
		metrics.ExportDuration,
		metrics.VacuumDuration,
	)

	return nil
}

func runWatchCommand(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexPath := fs.String("path", "", "Root path for this index (must match the original -path)")
	indexName := fs.String("name", "", "Name of the existing index (required)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	recursive := fs.Bool("recursive", true, "Recursively refresh directories on change events")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	var watchPaths stringSliceFlag
	fs.Var(&watchPaths, "watch-path", "Directory to watch for filesystem events (repeatable, required)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	if *indexName == "" {
		fs.Usage()
		return fmt.Errorf("-name is required")
	}
	if len(watchPaths) == 0 {
		fs.Usage()
		return fmt.Errorf("at least one -watch-path is required")
	}

	dbPath := deriveDBPath(*dbPathFlag)
	rootPath := *indexPath
	if rootPath == "" {
		rootPath = "/"
	}

	idx := indexing.Initialize(*indexName, rootPath, rootPath, false)

	return runWatchMode(idx, *indexName, dbPath, watchPaths, *recursive)
}

func runWatchMode(idx *indexing.Index, indexName, dbPath string, watchPaths []string, recursive bool) error {
	if dbPath == "" {
		return fmt.Errorf("watch mode requires -db-path or INDEXER_DB_PATH")
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	addWatch := func(path string) {
		if err := watcher.Add(path); err != nil {
			logger.Warnf("watch: unable to watch %s: %v", path, err)
		} else {
			logger.Debugf("watch: now watching %s", path)
		}
	}

	for _, raw := range watchPaths {
		absPath, err := filepath.Abs(raw)
		if err != nil {
			logger.Warnf("watch: unable to resolve %s: %v", raw, err)
			continue
		}
		if recursive {
			err = filepath.Walk(absPath, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					logger.Warnf("watch: walk error on %s: %v", path, walkErr)
					return nil
				}
				if info != nil && info.IsDir() {
					addWatch(path)
				}
				return nil
			})
			if err != nil {
				logger.Warnf("watch: unable to walk %s: %v", absPath, err)
			}
		} else {
			addWatch(absPath)
		}
	}

	logger.Infof("watch: started event-driven refresh for index %s on paths: %s", indexName, strings.Join(watchPaths, ", "))

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}

			if recursive && event.Op&fsnotify.Create == fsnotify.Create {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					addWatch(event.Name)
				}
			}

			target := strings.TrimSpace(event.Name)
			if target == "" {
				continue
			}

			logger.Debugf("watch: filesystem event %s on %s", event.Op.String(), target)

			err := runRefreshMode(idx, indexName, dbPath, []string{target}, recursive)
			if err != nil {
				logger.Errorf("watch: refresh for %s failed: %v", target, err)
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logger.Errorf("watch: error: %v", err)
		}
	}
}

func runDaemonCommand() error {
	// Daemon mode: long-lived process that periodically runs a full index
	// and listens for filesystem events on a small set of high-value paths.
	initLogger(false)

	indexPath := "/"
	name := deriveIndexName("", indexPath)
	dbPath := deriveDBPath("")

	watchPaths := []string{"/home", "/var/log"}
	filtered := make([]string, 0, len(watchPaths))
	for _, p := range watchPaths {
		if _, err := os.Stat(p); err == nil {
			filtered = append(filtered, p)
		}
	}

	if len(filtered) > 0 {
		go func() {
			idx := indexing.Initialize(name, indexPath, indexPath, false)
			if err := runWatchMode(idx, name, dbPath, filtered, true); err != nil {
				logger.Errorf("daemon: watch mode exited: %v", err)
			}
		}()
	}

	// Initial full index at startup
	if err := indexOnce(indexPath, name, dbPath, false, false); err != nil {
		logger.Errorf("daemon: initial index failed: %v", err)
	}

	// Periodic full index (hard-coded schedule)
	const fullScanInterval = 15 * time.Minute
	ticker := time.NewTicker(fullScanInterval)
	defer ticker.Stop()

	logger.Infof("daemon: started (path=%s, index=%s, db=%s, interval=%v)", indexPath, name, dbPath, fullScanInterval)

	for range ticker.C {
		if err := indexOnce(indexPath, name, dbPath, false, false); err != nil {
			logger.Errorf("daemon: periodic index failed: %v", err)
		}
	}

	return nil
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("refresh-path cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func (s *stringSliceFlag) Append(value string) {
	*s = append(*s, value)
}

func deriveIndexName(flagName, path string) string {
	if flagName != "" {
		return flagName
	}
	name := strings.ReplaceAll(path, "/", "_")
	if name == "" || name == "_" {
		name = "root"
	}
	return name
}

func deriveDBPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	if env := os.Getenv("INDEXER_DB_PATH"); env != "" {
		return env
	}
	return "indexer.db"
}

func runSearchCommand(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexName := fs.String("name", "", "Name of the existing index (required)")
	indexPath := fs.String("path", "", "Root path used when creating the index (optional)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	query := fs.String("query", "", "Search term to find files/folders (required)")
	caseSensitive := fs.Bool("case-sensitive", false, "Perform case-sensitive search")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files and directories (starting with .)")
	jsonOutput := fs.Bool("json", false, "Output results as JSON instead of human-readable text")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	// Allow a positional search term:
	//   indexer search cockpit
	// is equivalent to:
	//   indexer search -query cockpit
	if *query == "" {
		remaining := fs.Args()
		if len(remaining) > 0 {
			*query = remaining[0]
		}
	}

	if *query == "" {
		fs.Usage()
		return fmt.Errorf("-query is required")
	}

	dbPath := deriveDBPath(*dbPathFlag)
	rootPath := *indexPath
	if rootPath == "" {
		rootPath = "/"
	}

	effectiveName := *indexName
	if effectiveName == "" {
		names, err := storage.ListIndexNames(context.Background(), dbPath)
		if err != nil {
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				return fmt.Errorf("-name is required (no indexes found in %s)", dbPath)
			}
			return fmt.Errorf("list indexes: %w", err)
		}
		if len(names) == 1 {
			effectiveName = names[0]
			logger.Infof("Using default index %q from %s", effectiveName, dbPath)
		} else {
			return fmt.Errorf("-name is required (multiple indexes found: %s)", strings.Join(names, ", "))
		}
	}

	// Initialize an empty index and load the snapshot from SQLite.
	idx := indexing.Initialize(effectiveName, rootPath, rootPath, *includeHidden)
	if err := storage.LoadSnapshotIntoIndex(context.Background(), dbPath, effectiveName, idx); err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	results := idx.Search(*query, *caseSensitive)

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		return nil
	}

	if len(results) == 0 {
		logger.Infof("No matches found for %q", *query)
		return nil
	}

	logger.Infof("Searching for: %s", *query)
	for _, result := range results {
		if result.IsDir {
			logger.Infof("Folder: %s", result.Path)
		} else {
			logger.Infof("File: %s (size: %d bytes)", result.Path, result.Size)
		}
	}
	logger.Infof("Total matches: %d", len(results))
	return nil
}

func runServeCommand(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	addr := fs.String("addr", ":10210", "HTTP listen address")
	defaultIndex := fs.String("default-index", "", "Default index name to use when none is specified in queries")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	dbPath := deriveDBPath(*dbPathFlag)
	if dbPath == "" {
		return fmt.Errorf("db path is required")
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			http.Error(w, "missing q parameter", http.StatusBadRequest)
			return
		}

		name := r.URL.Query().Get("name")
		if name == "" {
			if *defaultIndex != "" {
				name = *defaultIndex
			} else {
				names, err := storage.ListIndexNamesFromDB(r.Context(), db)
				if err != nil {
					status := http.StatusInternalServerError
					if errors.Is(err, storage.ErrSnapshotNotFound) {
						status = http.StatusNotFound
					}
					http.Error(w, err.Error(), status)
					return
				}
				if len(names) == 1 {
					name = names[0]
					logger.Infof("HTTP /search: using default index %q from %s", name, dbPath)
				} else {
					http.Error(w, "missing name parameter and multiple indexes exist", http.StatusBadRequest)
					return
				}
			}
		}

		caseSensitive := r.URL.Query().Get("caseSensitive") == "true"

		idx := indexing.Initialize(name, "/", "/", false)
		if err := storage.LoadSnapshotIntoIndex(r.Context(), dbPath, name, idx); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}

		results := idx.Search(q, caseSensitive)

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(results); err != nil {
			http.Error(w, "encode response failed", http.StatusInternalServerError)
			return
		}
	})

	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		qPath := strings.TrimSpace(r.URL.Query().Get("path"))
		if qPath == "" {
			http.Error(w, "missing path parameter", http.StatusBadRequest)
			return
		}

		name := r.URL.Query().Get("name")
		if name == "" {
			if *defaultIndex != "" {
				name = *defaultIndex
			} else {
				names, err := storage.ListIndexNames(r.Context(), dbPath)
				if err != nil {
					status := http.StatusInternalServerError
					if errors.Is(err, storage.ErrSnapshotNotFound) {
						status = http.StatusNotFound
					}
					http.Error(w, err.Error(), status)
					return
				}
				if len(names) == 1 {
					name = names[0]
					logger.Infof("HTTP /stats: using default index %q from %s", name, dbPath)
				} else {
					http.Error(w, "missing name parameter and multiple indexes exist", http.StatusBadRequest)
					return
				}
			}
		}

		stats, err := storage.GetDirStatsFromDB(r.Context(), db, name, qPath)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(stats); err != nil {
			http.Error(w, "encode response failed", http.StatusInternalServerError)
			return
		}
	})

	mux.HandleFunc("/size", func(w http.ResponseWriter, r *http.Request) {
		qPath := strings.TrimSpace(r.URL.Query().Get("path"))
		if qPath == "" {
			http.Error(w, "missing path parameter", http.StatusBadRequest)
			return
		}

		name := r.URL.Query().Get("name")
		if name == "" {
			if *defaultIndex != "" {
				name = *defaultIndex
			} else {
				names, err := storage.ListIndexNames(r.Context(), dbPath)
				if err != nil {
					status := http.StatusInternalServerError
					if errors.Is(err, storage.ErrSnapshotNotFound) {
						status = http.StatusNotFound
					}
					http.Error(w, err.Error(), status)
					return
				}
				if len(names) == 1 {
					name = names[0]
					logger.Infof("HTTP /size: using default index %q from %s", name, dbPath)
				} else {
					http.Error(w, "missing name parameter and multiple indexes exist", http.StatusBadRequest)
					return
				}
			}
		}

		stats, err := storage.GetDirStatsFromDB(r.Context(), db, name, qPath)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}

		type sizeResponse struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}

		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		if err := enc.Encode(sizeResponse{Path: stats.RelativePath, Size: stats.Size}); err != nil {
			http.Error(w, "encode response failed", http.StatusInternalServerError)
			return
		}
	})

	logger.Infof("Starting HTTP API on %s (db=%s)", *addr, dbPath)
	logger.Infof("Available endpoints:")
	logger.Infof("  GET /health")
	logger.Infof("  GET /search?q=<term>&name=<index>&caseSensitive=true|false")
	logger.Infof("  GET /stats?path=<dir>&name=<index>")
	logger.Infof("  GET /size?path=<dir>&name=<index>")
	return http.ListenAndServe(*addr, mux)
}

func runSocketCommand(args []string) error {
	fs := flag.NewFlagSet("socket", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	socketPath := fs.String("socket", "/run/indexer.sock", "Unix socket path to listen on (ignored when started by systemd socket activation)")
	defaultIndex := fs.String("default-index", "", "Default index name to use when none is specified")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	logger.Init("production", *verbose)

	dbPath := deriveDBPath(*dbPathFlag)
	if dbPath == "" {
		return fmt.Errorf("db path is required")
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = db.Close() }()

	ln, usedSystemd, err := listenUnixOrSystemd(*socketPath)
	if err != nil {
		return fmt.Errorf("listen on socket: %w", err)
	}
	defer func() { _ = ln.Close() }()

	if usedSystemd {
		logger.Infof("Starting Unix socket server via systemd activation (db=%s)", dbPath)
	} else {
		logger.Infof("Starting Unix socket server on %s (db=%s)", *socketPath, dbPath)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			logger.Errorf("accept error: %v", err)
			continue
		}
		go handleSocketConn(conn, db, dbPath, *defaultIndex)
	}
}

func listenUnixOrSystemd(path string) (net.Listener, bool, error) {
	// systemd socket activation: LISTEN_FDS >= 1, LISTEN_PID == our pid.
	if fdsStr := os.Getenv("LISTEN_FDS"); fdsStr != "" {
		fds, err := strconv.Atoi(fdsStr)
		if err == nil && fds > 0 {
			if pidStr := os.Getenv("LISTEN_PID"); pidStr != "" {
				if pid, err2 := strconv.Atoi(pidStr); err2 == nil && pid == os.Getpid() {
					// First socket is fd 3.
					f := os.NewFile(uintptr(3), "systemd-unix-socket")
					ln, err := net.FileListener(f)
					if err != nil {
						return nil, false, err
					}
					return ln, true, nil
				}
			}
		}
	}

	// Fallback: manage our own Unix socket path.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	return ln, false, err
}

func handleSocketConn(conn net.Conn, db *sql.DB, dbPath string, defaultIndex string) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewScanner(conn)
	for reader.Scan() {
		line := strings.TrimSpace(reader.Text())
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		cmd := strings.ToUpper(fields[0])
		args := fields[1:]

		switch cmd {
		case "SIZE":
			handleSocketSize(conn, db, defaultIndex, args)
		case "STATS":
			handleSocketStats(conn, db, defaultIndex, args)
		case "SEARCH":
			handleSocketSearch(conn, db, dbPath, defaultIndex, args)
		default:
			if _, err := fmt.Fprintf(conn, "ERR unknown command: %s\n", cmd); err != nil {
				logger.Errorf("socket write error: %v", err)
				return
			}
		}
	}
}

func effectiveIndexNameFromDB(ctx context.Context, db *sql.DB, defaultIndex string) (string, error) {
	if defaultIndex != "" {
		return defaultIndex, nil
	}

	names, err := storage.ListIndexNamesFromDB(ctx, db)
	if err != nil {
		return "", err
	}
	if len(names) == 1 {
		return names[0], nil
	}
	if len(names) == 0 {
		return "", storage.ErrSnapshotNotFound
	}
	return "", fmt.Errorf("multiple indexes exist; specify an explicit index")
}

func handleSocketSize(conn net.Conn, db *sql.DB, defaultIndex string, args []string) {
	if len(args) == 0 {
		if _, err := fmt.Fprintln(conn, "ERR SIZE requires a path"); err != nil {
			logger.Errorf("socket write error: %v", err)
		}
		return
	}
	path := args[0]

	ctx := context.Background()
	name, err := effectiveIndexNameFromDB(ctx, db, defaultIndex)
	if err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	stats, err := storage.GetDirStatsFromDB(ctx, db, name, path)
	if err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	if _, err := fmt.Fprintf(conn, "%s %d\n", stats.RelativePath, stats.Size); err != nil {
		logger.Errorf("socket write error: %v", err)
	}
}

func handleSocketStats(conn net.Conn, db *sql.DB, defaultIndex string, args []string) {
	if len(args) == 0 {
		if _, err := fmt.Fprintln(conn, "ERR STATS requires a path"); err != nil {
			logger.Errorf("socket write error: %v", err)
		}
		return
	}
	path := args[0]

	ctx := context.Background()
	name, err := effectiveIndexNameFromDB(ctx, db, defaultIndex)
	if err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	stats, err := storage.GetDirStatsFromDB(ctx, db, name, path)
	if err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	type jsonStats struct {
		Path     string    `json:"path"`
		Size     int64     `json:"size"`
		NumDirs  int64     `json:"numDirs"`
		NumFiles int64     `json:"numFiles"`
		ModTime  time.Time `json:"modTime"`
	}
	out := jsonStats{
		Path:     stats.RelativePath,
		Size:     stats.Size,
		NumDirs:  stats.NumDirs,
		NumFiles: stats.NumFiles,
		ModTime:  stats.ModTime,
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(out); err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR encode: %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}
}

func handleSocketSearch(conn net.Conn, db *sql.DB, dbPath string, defaultIndex string, args []string) {
	if len(args) == 0 {
		if _, err := fmt.Fprintln(conn, "ERR SEARCH requires a term"); err != nil {
			logger.Errorf("socket write error: %v", err)
		}
		return
	}
	term := args[0]

	ctx := context.Background()
	name, err := effectiveIndexNameFromDB(ctx, db, defaultIndex)
	if err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	// Use the existing snapshot-loading path (opens its own read-only handle).
	idx := indexing.Initialize(name, "/", "/", false)
	if err := storage.LoadSnapshotIntoIndex(ctx, dbPath, name, idx); err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}

	results := idx.Search(term, false)

	enc := json.NewEncoder(conn)
	if err := enc.Encode(results); err != nil {
		if _, werr := fmt.Fprintf(conn, "ERR encode: %v\n", err); werr != nil {
			logger.Errorf("socket write error: %v", werr)
		}
		return
	}
}

func runStatsCommand(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexName := fs.String("name", "", "Name of the existing index (optional when only one index exists)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	path := fs.String("path", "", "Directory path inside the index (e.g. /, /var/log, /home/user)")
	jsonOutput := fs.Bool("json", false, "Output results as JSON instead of human-readable text")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	if *path == "" {
		// Allow positional path: `indexer stats /var/log`
		remaining := fs.Args()
		if len(remaining) > 0 {
			*path = remaining[0]
		}
	}
	if *path == "" {
		fs.Usage()
		return fmt.Errorf("-path is required")
	}

	dbPath := deriveDBPath(*dbPathFlag)

	effectiveName := *indexName
	if effectiveName == "" {
		names, err := storage.ListIndexNames(context.Background(), dbPath)
		if err != nil {
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				return fmt.Errorf("-name is required (no indexes found in %s)", dbPath)
			}
			return fmt.Errorf("list indexes: %w", err)
		}
		if len(names) == 1 {
			effectiveName = names[0]
			logger.Infof("Using default index %q from %s", effectiveName, dbPath)
		} else {
			return fmt.Errorf("-name is required (multiple indexes found: %s)", strings.Join(names, ", "))
		}
	}

	stats, err := storage.GetDirStats(context.Background(), dbPath, effectiveName, *path)
	if err != nil {
		return fmt.Errorf("get stats: %w", err)
	}

	if *jsonOutput {
		type jsonStats struct {
			Path     string    `json:"path"`
			Size     int64     `json:"size"`
			NumDirs  int64     `json:"numDirs"`
			NumFiles int64     `json:"numFiles"`
			ModTime  time.Time `json:"modTime"`
		}
		out := jsonStats{
			Path:     stats.RelativePath,
			Size:     stats.Size,
			NumDirs:  stats.NumDirs,
			NumFiles: stats.NumFiles,
			ModTime:  stats.ModTime,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		return nil
	}

	fmt.Printf("Directory: %s\n", stats.RelativePath)
	fmt.Printf("Size: %d bytes (%.2f GB)\n", stats.Size, float64(stats.Size)/(1024*1024*1024))
	fmt.Printf("Subdirectories (recursive): %d\n", stats.NumDirs)
	fmt.Printf("Files (recursive): %d\n", stats.NumFiles)
	fmt.Printf("Last modified: %s\n", stats.ModTime.Format(time.RFC3339))
	return nil
}

func runSizeCommand(args []string) error {
	fs := flag.NewFlagSet("size", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	indexName := fs.String("name", "", "Name of the existing index (optional when only one index exists)")
	dbPathFlag := fs.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	path := fs.String("path", "", "Directory path inside the index (e.g. /, /var/log, /home/user)")
	verbose := fs.Bool("verbose", false, "Enable verbose logging (DEBUG level)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	initLogger(*verbose)

	// Collect all target paths: optional -path plus any positional arguments.
	paths := []string{}
	if *path != "" {
		paths = append(paths, *path)
	}
	remaining := fs.Args()
	if len(remaining) > 0 {
		paths = append(paths, remaining...)
	}
	if len(paths) == 0 {
		fs.Usage()
		return fmt.Errorf("-path is required")
	}

	dbPath := deriveDBPath(*dbPathFlag)

	effectiveName := *indexName
	if effectiveName == "" {
		names, err := storage.ListIndexNames(context.Background(), dbPath)
		if err != nil {
			if errors.Is(err, storage.ErrSnapshotNotFound) {
				return fmt.Errorf("-name is required (no indexes found in %s)", dbPath)
			}
			return fmt.Errorf("list indexes: %w", err)
		}
		if len(names) == 1 {
			effectiveName = names[0]
			logger.Infof("Using default index %q from %s", effectiveName, dbPath)
		} else {
			return fmt.Errorf("-name is required (multiple indexes found: %s)", strings.Join(names, ", "))
		}
	}

	ctx := context.Background()

	// Single path: preserve the simple "just bytes" output.
	if len(paths) == 1 {
		stats, err := storage.GetDirStats(ctx, dbPath, effectiveName, paths[0])
		if err != nil {
			return fmt.Errorf("get size: %w", err)
		}
		fmt.Printf("%d\n", stats.Size)
		return nil
	}

	// Multiple paths: print "path size" per line.
	for _, p := range paths {
		stats, err := storage.GetDirStats(ctx, dbPath, effectiveName, p)
		if err != nil {
			return fmt.Errorf("get size for %s: %w", p, err)
		}
		fmt.Printf("%s %d\n", stats.RelativePath, stats.Size)
	}
	return nil
}

func normalizeTargetPath(root, candidate string) (string, error) {
	if candidate == "" {
		return "", fmt.Errorf("path cannot be empty")
	}
	cleanRoot := filepath.Clean(root)
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(cleanRoot, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if abs != cleanRoot && !strings.HasPrefix(abs, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %s is outside indexed root %s", abs, cleanRoot)
	}
	return abs, nil
}

type rateLimiter struct {
	path        string
	cooldown    time.Duration
	clock       func() time.Time
	lastRunPath string
}

func newRateLimiter(dbPath string) *rateLimiter {
	return &rateLimiter{
		path:     dbPath,
		cooldown: 30 * time.Second,
		clock:    time.Now,
	}
}

func (r *rateLimiter) Enforce() error {
	if r.path == "" {
		return nil
	}
	statePath := r.stateFilePath()
	data, err := os.ReadFile(statePath)
	if err == nil {
		lastUnix, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if parseErr == nil {
			last := time.Unix(lastUnix, 0)
			if remaining := r.cooldown - r.clock().Sub(last); remaining > 0 {
				return fmt.Errorf("indexer recently ran; retry in %v", remaining.Round(time.Second))
			}
		}
	}

	tmp := fmt.Sprintf("%d", r.clock().Unix())
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(statePath, []byte(tmp), 0o644)
}

func (r *rateLimiter) stateFilePath() string {
	if r.lastRunPath != "" {
		return r.lastRunPath
	}
	base := filepath.Base(r.path)
	if base == "." || base == "" {
		base = "indexer"
	}
	file := fmt.Sprintf(".%s.last_run", base)
	r.lastRunPath = filepath.Join(os.TempDir(), "indexer", file)
	return r.lastRunPath
}
