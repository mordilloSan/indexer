package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/cmd"
	"github.com/mordilloSan/indexer/internal/version"
)

func main() {
	var (
		indexMode     = flag.Bool("index-mode", false, "Internal: run index and exit (spawned by daemon)")
		showVersion   = flag.Bool("version", false, "Print version and exit")
		showStatus    = flag.Bool("status", false, "Query /status from a running daemon and exit")
		indexPath     = flag.String("path", "", "Path to index (required)")
		indexName     = flag.String("name", "", "Name for this index (defaults to sanitized path)")
		includeHidden = flag.Bool("include-hidden", false, "Include hidden files and directories")
		dbPath        = flag.String("db-path", "", "SQLite database path (overrides INDEXER_DB_PATH)")
		socketPath    = flag.String("socket-path", "/var/run/indexer.sock", "Unix socket path")
		listenAddr    = flag.String("listen", "", "Optional TCP address (e.g., :8080)")
		indexInterval = flag.String("interval", "1h", "Auto-index interval (Go duration like 6h, 30m); 0 disables")
		verbose       = flag.Bool("verbose", false, "Enable verbose logging")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	if *showStatus {
		if err := printStatus(*socketPath, *listenAddr); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		return
	}

	logger.Init("production", *verbose)
	mode := "daemon"
	if *indexMode {
		mode = "index"
	}
	logger.Infof("%s starting mode=%s", version.String(), mode)

	if *indexPath == "" {
		logger.Errorf("Error: -path flag is required")
		flag.Usage()
		os.Exit(1)
	}

	nameVal := sanitizeName(*indexName, *indexPath)
	dbVal := coalesce(*dbPath, os.Getenv("INDEXER_DB_PATH"), "/tmp/indexer.db")

	// If running in index mode, do index and exit
	if *indexMode {
		cmd.RunIndexMode(nameVal, *indexPath, *includeHidden, dbVal, *verbose)
		return
	}

	// Otherwise, run daemon normally
	socketVal := coalesce(*socketPath, "/var/run/indexer.sock")
	if *socketPath == "-" {
		socketVal = "-"
	}
	listenVal := *listenAddr

	interval, err := parseInterval(*indexInterval)
	if err != nil {
		logger.Warnf("Invalid interval %q, defaulting to 0 (disabled): %v", *indexInterval, err)
		interval = 0
	}

	cfg := cmd.DaemonConfig{
		IndexName:     nameVal,
		IndexPath:     *indexPath,
		IncludeHidden: *includeHidden,
		DBPath:        dbVal,
		SocketPath:    socketVal,
		ListenAddr:    listenVal,
		Interval:      interval,
	}

	d, err := cmd.NewDaemon(cfg)
	if err != nil {
		logger.Fatalf("Failed to start daemon: %v", err)
	}
	defer d.Close()

	// Log daemon configuration
	listenDisplay := cfg.ListenAddr
	if listenDisplay == "" {
		listenDisplay = "disabled"
	}
	logger.Infof("Daemon initialized path=%s name=%s db=%s socket=%s listen=%s includeHidden=%t interval=%v",
		cfg.IndexPath, cfg.IndexName, cfg.DBPath, cfg.SocketPath, listenDisplay, cfg.IncludeHidden, cfg.Interval)
	if *dbPath == "" && os.Getenv("INDEXER_DB_PATH") == "" {
		logger.Warnf("DB path not set; defaulting to %s", cfg.DBPath)
	}
	if *socketPath == "" {
		logger.Warnf("Socket path empty; defaulting to %s", cfg.SocketPath)
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

func parseInterval(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func printStatus(socketPath, listenAddr string) error {
	client, url, err := statusClientAndURL(socketPath, listenAddr)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := ioReadAllLimit(resp.Body, 2<<20)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	fmt.Println(strings.TrimSpace(string(body)))
	return nil
}

func statusClientAndURL(socketPath, listenAddr string) (*http.Client, string, error) {
	timeout := 5 * time.Second

	if listenAddr != "" {
		base := listenAddr
		if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
			return &http.Client{Timeout: timeout}, strings.TrimRight(base, "/") + "/status", nil
		}
		if strings.HasPrefix(base, ":") {
			base = "127.0.0.1" + base
		}
		return &http.Client{Timeout: timeout}, "http://" + strings.TrimRight(base, "/") + "/status", nil
	}

	if socketPath == "-" {
		return nil, "", fmt.Errorf("status requires either --listen or a unix socket (got --socket-path '-')")
	}
	if socketPath == "" {
		socketPath = "/var/run/indexer.sock"
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &http.Client{Transport: tr, Timeout: timeout}, "http://unix/status", nil
}

func ioReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: limit}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("response too large (>%d bytes)", limit)
	}
	return b, nil
}
