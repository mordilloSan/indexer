package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/mordilloSan/go_logger/logger"

	"indexer/cmd"
)

func main() {
	var (
		indexPath       = flag.String("path", "", "Path to index (required)")
		indexName       = flag.String("name", "", "Name for this index (defaults to sanitized path)")
		includeHidden   = flag.Bool("include-hidden", false, "Include hidden files and directories")
		dbPath          = flag.String("db-path", "", "SQLite database path (overrides INDEXER_DB_PATH)")
		socketPath      = flag.String("socket-path", "/var/run/indexer.sock", "Unix socket path")
		listenAddr      = flag.String("listen", "", "Optional TCP address (e.g., :8080)")
		reindexInterval = flag.Duration("interval", 0, "Auto-reindex interval (e.g., 6h), 0=disabled")
		verbose         = flag.Bool("verbose", false, "Enable verbose logging")
	)
	flag.Parse()

	logger.Init("development", *verbose)

	if *indexPath == "" {
		logger.Errorf("Error: -path flag is required")
		flag.Usage()
		os.Exit(1)
	}

	cfg := cmd.DaemonConfig{
		IndexName:     sanitizeName(*indexName, *indexPath),
		IndexPath:     *indexPath,
		IncludeHidden: *includeHidden,
		DBPath:        coalesce(*dbPath, os.Getenv("INDEXER_DB_PATH"), "/tmp/indexer.db"),
		SocketPath:    *socketPath,
		ListenAddr:    *listenAddr,
		Interval:      *reindexInterval,
	}

	d, err := cmd.NewDaemon(cfg)
	if err != nil {
		logger.Fatalf("Failed to start daemon: %v", err)
	}
	defer d.Close()

	// Log startup configuration
	logger.Infof("Starting indexer daemon")
	if cfg.Interval > 0 {
		logger.Infof("Auto-reindex: every %v", cfg.Interval)
	}
	if cfg.IncludeHidden {
		logger.Infof("Including hidden files")
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for interrupt signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// Run daemon in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	// Wait for shutdown signal or error
	select {
	case sig := <-sigCh:
		logger.Infof("Received signal %v, initiating graceful shutdown...", sig)
		cancel() // Trigger context cancellation
		<-errCh  // Wait for daemon to finish
	case err := <-errCh:
		if err != nil {
			logger.Fatalf("Daemon exited with error: %v", err)
		}
	}

	logger.Infof("Shutdown complete")
}

func sanitizeName(name, path string) string {
	if name != "" {
		return name
	}
	name = strings.ReplaceAll(path, "/", "_")
	if name == "" || name == "_" {
		return "root"
	}
	return strings.Trim(name, "_")
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
