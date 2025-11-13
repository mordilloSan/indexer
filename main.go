package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mordilloSan/go_logger/logger"
	"indexer/indexing"
	"indexer/storage"
)

func main() {
	// Define command-line flags
	indexPath := flag.String("path", "", "Path to index (required, e.g., /, /home, /var)")
	indexName := flag.String("name", "", "Name for this index (optional, defaults to path)")
	searchTerm := flag.String("search", "", "Search term to find files/folders after indexing (optional)")
	caseSensitive := flag.Bool("case-sensitive", false, "Perform case-sensitive search")
	verbose := flag.Bool("verbose", false, "Enable verbose logging (DEBUG level)")
	includeHidden := flag.Bool("include-hidden", false, "Include hidden files and directories (starting with .)")
	dbPathFlag := flag.String("db-path", "", "Path to the SQLite database file (overrides INDEXER_DB_PATH)")
	resumeFlag := flag.Bool("resume", false, "Reuse the previous database snapshot to speed up re-indexing")
	refreshRecursive := flag.Bool("refresh-recursive", false, "Recursively refresh directories supplied via -refresh-path")

	var refreshPaths stringSliceFlag
	flag.Var(&refreshPaths, "refresh-path", "Absolute file or directory to refresh without a full scan (repeatable)")

	flag.Parse()

	// Initialize logger (development mode with optional verbose debug)
	logger.Init("development", *verbose)

	// Validate required flags
	if *indexPath == "" {
		logger.Errorf("Error: -path flag is required")
		fmt.Println("\nUsage:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Set default name if not provided
	name := *indexName
	if name == "" {
		name = strings.ReplaceAll(*indexPath, "/", "_")
		if name == "" || name == "_" {
			name = "root"
		}
	}

	dbPath := *dbPathFlag
	if dbPath == "" {
		dbPath = os.Getenv("INDEXER_DB_PATH")
	}
	if dbPath == "" {
		dbPath = "indexer.db"
	}

	// Initialize the indexer
	logger.Infof("Initializing indexer for path: %s", *indexPath)
	index := indexing.Initialize(name, *indexPath, *indexPath, *includeHidden)

	if len(refreshPaths) > 0 {
		if err := runRefreshMode(index, name, dbPath, refreshPaths, *refreshRecursive); err != nil {
			logger.Errorf("Refresh failed: %v", err)
			os.Exit(1)
		}
		return
	}

	if *resumeFlag {
		if err := storage.LoadSnapshotIntoIndex(context.Background(), dbPath, name, index); err != nil {
			if !errors.Is(err, storage.ErrSnapshotNotFound) {
				logger.Warnf("Unable to load previous snapshot: %v", err)
			} else {
				logger.Debugf("No previous snapshot found for %s", name)
			}
		} else {
			logger.Infof("Loaded previous snapshot for %s; quick scan enabled", name)
		}
	}

	rateLimiter := newRateLimiter(dbPath)
	if err := rateLimiter.Enforce(); err != nil {
		logger.Errorf("Aborting: %v", err)
		os.Exit(1)
	}

	// Start indexing with timer
	logger.Infof("Starting indexing...")
	startTime := time.Now()
	err := index.StartIndexing()
	duration := time.Since(startTime)

	if err != nil {
		logger.Errorf("Error during indexing: %v", err)
		os.Exit(1)
	}

	// Print summary
	logger.Infof("Indexing completed successfully!")
	logger.Infof("Total directories: %d", index.NumDirs)
	logger.Infof("Total files: %d", index.NumFiles)
	logger.Infof("Total size: %d bytes (%.2f GB)", index.GetTotalSize(), float64(index.GetTotalSize())/(1024*1024*1024))
	logger.Infof("Indexing duration: %v", duration)

	saveCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	metrics := &storage.PersistMetrics{IndexDuration: duration}
	if err := storage.SaveIndexToFile(saveCtx, dbPath, index, metrics); err != nil {
		logger.Errorf("Failed to persist index: %v", err)
		os.Exit(1)
	}
	logger.Infof(
		"Saved index '%s' to database (export: %v, vacuum: %v)",
		index.Name,
		metrics.ExportDuration,
		metrics.VacuumDuration,
	)

	// Perform search if search term provided
	if *searchTerm != "" {
		logger.Infof("Searching for: %s", *searchTerm)
		results := index.Search(*searchTerm, *caseSensitive)

		if len(results) == 0 {
			logger.Infof("No matches found")
		} else {
			for _, result := range results {
				if result.IsDir {
					logger.Infof("  Folder: %s", result.Path)
				} else {
					logger.Infof("  File: %s (size: %d bytes)", result.Path, result.Size)
				}
			}
			logger.Infof("Total matches: %d", len(results))
		}
	}
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
