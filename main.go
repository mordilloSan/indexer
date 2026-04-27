package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mordilloSan/indexer/cmd"
	"github.com/mordilloSan/indexer/internal/version"
)

const usageText = `Usage: indexer <command> [flags]

Commands:
  daemon    Start the indexer daemon
  index     Index one folder and exit
  status    Query status from a running daemon
  config    Show configuration of a running daemon
  version   Print version information

Run 'indexer <command> --help' for details on a specific command.
`

func main() {
	// Internal: handle --index-mode for subprocess spawning (not shown in help)
	if len(os.Args) > 1 && os.Args[1] == "--index-mode" {
		runInternalIndexMode(os.Args[2:])
		return
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daemon":
		runDaemon(os.Args[2:])
	case "index":
		runIndex(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "config":
		runConfig(os.Args[2:])
	case "version":
		fmt.Println(version.String())
	case "--help", "-h", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	indexPath := fs.String("path", "", "Path to index (required)")
	indexName := fs.String("name", "", "Name for this index (defaults to sanitized path)")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files and directories")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network/external mounts such as NFS, SMB, and CIFS")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "Alias for --include-network-mounts")
	freshIndex := fs.Bool("fresh", true, "Clear database before each full index (no previous results kept)")
	keepIndexes := fs.Int("keep-indexes", 0, "Most recent index records to keep after indexing (0 disables automatic pruning)")
	dbPath := fs.String("db-path", "", "SQLite database path (overrides INDEXER_DB_PATH)")
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path (\"-\" to disable)")
	listenAddr := fs.String("listen", "", "Optional TCP address (e.g., :8080)")
	indexInterval := fs.String("interval", "1h", "Auto-index interval (Go duration like 6h, 30m); 0 disables")
	verbose := fs.Bool("verbose", false, "Enable verbose logging")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	configureLogger(*verbose)
	slog.Info("indexer starting", "version", version.String(), "mode", "daemon")

	if *indexPath == "" {
		slog.Error("missing required flag", "flag", "--path")
		fs.Usage()
		os.Exit(1)
	}

	nameVal := sanitizeName(*indexName, *indexPath)
	dbVal := coalesce(*dbPath, os.Getenv("INDEXER_DB_PATH"), "/tmp/indexer.db")
	freshVal := *freshIndex
	if !flagWasSet(fs, "fresh") && strings.TrimSpace(os.Getenv("INDEXER_FRESH")) != "" {
		freshVal = envBool("INDEXER_FRESH")
	}
	includeNetworkMountsVal := *includeNetworkMounts
	if !flagWasSet(fs, "include-network-mounts") && !flagWasSet(fs, "include-external-mounts") {
		includeNetworkMountsVal = envBool("INDEXER_INCLUDE_NETWORK_MOUNTS") || envBool("INDEXER_INCLUDE_EXTERNAL_MOUNTS")
	}
	keepIndexesVal := *keepIndexes
	if !flagWasSet(fs, "keep-indexes") {
		keepIndexesVal = envInt("INDEXER_KEEP_INDEXES", keepIndexesVal)
	}

	socketVal := coalesce(*socketPath, "/var/run/indexer.sock")
	if *socketPath == "-" {
		socketVal = "-"
	}
	listenVal := *listenAddr

	interval, err := parseInterval(*indexInterval)
	if err != nil {
		slog.Warn("invalid interval, defaulting to disabled", "interval", *indexInterval, "err", err)
		interval = 0
	}

	cfg := cmd.DaemonConfig{
		IndexName:            nameVal,
		IndexPath:            *indexPath,
		IncludeHidden:        *includeHidden,
		IncludeNetworkMounts: includeNetworkMountsVal,
		FreshIndex:           freshVal,
		KeepIndexes:          keepIndexesVal,
		DBPath:               dbVal,
		SocketPath:           socketVal,
		ListenAddr:           listenVal,
		Interval:             interval,
	}

	d, err := cmd.NewDaemon(cfg)
	if err != nil {
		slog.Error("failed to start daemon", "err", err)
		os.Exit(1)
	}
	defer d.Close()

	// Log daemon configuration
	listenDisplay := cfg.ListenAddr
	if listenDisplay == "" {
		listenDisplay = "disabled"
	}
	slog.Info("daemon initialized",
		"path", cfg.IndexPath,
		"name", cfg.IndexName,
		"db", cfg.DBPath,
		"socket", cfg.SocketPath,
		"listen", listenDisplay,
		"include_hidden", cfg.IncludeHidden,
		"include_network_mounts", cfg.IncludeNetworkMounts,
		"keep_indexes", cfg.KeepIndexes,
		"interval", cfg.Interval,
	)
	if *dbPath == "" && os.Getenv("INDEXER_DB_PATH") == "" {
		slog.Warn("DB path not set; using default", "db", cfg.DBPath)
	}
	if *socketPath == "" {
		slog.Warn("socket path empty; using default", "socket", cfg.SocketPath)
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Run(ctx)
	}()

	select {
	case sig := <-sigCh:
		slog.Info("received signal, initiating graceful shutdown", "signal", sig)
		cancel()
		<-errCh
	case err := <-errCh:
		if err != nil {
			slog.Error("daemon exited with error", "err", err)
			os.Exit(1)
		}
	}

	slog.Info("shutdown complete")
}

func runIndex(args []string) {
	runIndexCommand("index", args)
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path")
	listenAddr := fs.String("listen", "", "TCP address of the daemon (e.g., :8080)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if err := queryDaemon(*socketPath, *listenAddr, "/status"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func runConfig(args []string) {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path")
	listenAddr := fs.String("listen", "", "TCP address of the daemon (e.g., :8080)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if err := queryDaemon(*socketPath, *listenAddr, "/config"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func runInternalIndexMode(args []string) {
	runIndexCommand("index-mode", args)
}

func runIndexCommand(flagSetName string, args []string) {
	fs := flag.NewFlagSet(flagSetName, flag.ExitOnError)
	indexPath := fs.String("path", "", "Path to index")
	indexName := fs.String("name", "", "Index name")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network/external mounts such as NFS, SMB, and CIFS")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "Alias for --include-network-mounts")
	freshIndex := fs.Bool("fresh", true, "Fresh index mode")
	keepIndexes := fs.Int("keep-indexes", 0, "Most recent index records to keep after indexing (0 disables automatic pruning)")
	dbPath := fs.String("db-path", "", "SQLite database path")
	cpuProfile := fs.String("cpu-profile", "", "Write CPU profile to file")
	verbose := fs.Bool("verbose", false, "Enable verbose logging")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	configureLogger(*verbose)
	slog.Info("indexer starting", "version", version.String(), "mode", "index")

	if *indexPath == "" {
		slog.Error("missing required flag", "flag", "--path")
		fs.Usage()
		os.Exit(1)
	}

	nameVal := sanitizeName(*indexName, *indexPath)
	dbVal := coalesce(*dbPath, os.Getenv("INDEXER_DB_PATH"), "/tmp/indexer.db")
	freshVal := *freshIndex
	if !flagWasSet(fs, "fresh") && strings.TrimSpace(os.Getenv("INDEXER_FRESH")) != "" {
		freshVal = envBool("INDEXER_FRESH")
	}
	includeNetworkMountsVal := *includeNetworkMounts
	if !flagWasSet(fs, "include-network-mounts") && !flagWasSet(fs, "include-external-mounts") {
		includeNetworkMountsVal = envBool("INDEXER_INCLUDE_NETWORK_MOUNTS") || envBool("INDEXER_INCLUDE_EXTERNAL_MOUNTS")
	}
	keepIndexesVal := *keepIndexes
	if !flagWasSet(fs, "keep-indexes") {
		keepIndexesVal = envInt("INDEXER_KEEP_INDEXES", keepIndexesVal)
	}

	if err := withCPUProfile(*cpuProfile, func() error {
		return cmd.RunIndexMode(nameVal, *indexPath, *includeHidden, includeNetworkMountsVal, freshVal, dbVal, keepIndexesVal)
	}); err != nil {
		slog.Error("index failed", "err", err)
		os.Exit(1)
	}
}

func withCPUProfile(path string, run func() error) error {
	if path == "" {
		return run()
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create cpu profile %s: %w", path, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close cpu profile: %v\n", closeErr)
		}
	}()

	if err := pprof.StartCPUProfile(f); err != nil {
		return fmt.Errorf("start cpu profile: %w", err)
	}
	defer pprof.StopCPUProfile()

	slog.Info("CPU profiling enabled", "path", path)
	return run()
}

// queryDaemon sends a GET request to the given endpoint on the running daemon and prints the response.
func queryDaemon(socketPath, listenAddr, endpoint string) error {
	client, url, err := buildClient(socketPath, listenAddr, endpoint)
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
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close response body: %v\n", closeErr)
		}
	}()

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

// buildClient returns an HTTP client and URL for reaching the daemon at the given endpoint.
func buildClient(socketPath, listenAddr, endpoint string) (*http.Client, string, error) {
	timeout := 5 * time.Second

	if listenAddr != "" {
		base := listenAddr
		if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
			return &http.Client{Timeout: timeout}, strings.TrimRight(base, "/") + endpoint, nil
		}
		if strings.HasPrefix(base, ":") {
			base = "127.0.0.1" + base
		}
		return &http.Client{Timeout: timeout}, "http://" + strings.TrimRight(base, "/") + endpoint, nil
	}

	if socketPath == "-" {
		return nil, "", fmt.Errorf("requires either --listen or a unix socket (got --socket-path '-')")
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
	return &http.Client{Transport: tr, Timeout: timeout}, "http://unix" + endpoint, nil
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

func configureLogger(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("invalid integer environment value", "name", name, "value", raw, "err", err)
		return fallback
	}
	if v < 0 {
		slog.Warn("negative integer environment value ignored", "name", name, "value", raw)
		return fallback
	}
	return v
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
