package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
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
	if len(args) > 0 && args[0] == "set" {
		runConfigSet(args[1:])
		return
	}
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path")
	listenAddr := fs.String("listen", "", "TCP address of the daemon (e.g., :8080)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	if err := queryDaemonPretty(*socketPath, *listenAddr, "/config"); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func runConfigSet(args []string) {
	const defaultEnvFile = "/etc/default/indexer"

	fs := flag.NewFlagSet("config set", flag.ExitOnError)
	envFile := fs.String("env-file", defaultEnvFile, "Environment file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	service := fs.String("service", "indexer", "Systemd service name")

	path := fs.String("path", "", "Path to index")
	name := fs.String("name", "", "Index name")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network mounts")
	fresh := fs.Bool("fresh", false, "Fresh index mode")
	keepIndexes := fs.Int("keep-indexes", 0, "Records to keep after indexing (0=disabled)")
	dbPath := fs.String("db-path", "", "SQLite database path")
	interval := fs.String("interval", "", "Auto-index interval (e.g. 1h, 30m, 0)")
	listen := fs.String("listen", "", "TCP listen address (e.g. :8080); empty to disable")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	updates := collectEnvUpdates(fs, path, name, includeHidden, includeNetworkMounts, fresh, keepIndexes, dbPath, interval, listen)

	if len(updates) == 0 {
		fmt.Fprintln(os.Stderr, "no settings specified; use flags like --interval=2h or --path=/data")
		fs.Usage()
		os.Exit(1)
	}

	if v, ok := updates["INDEXER_INTERVAL"]; ok {
		if _, err := parseInterval(v); err != nil {
			fmt.Fprintf(os.Stderr, "invalid interval %q: %v\n", v, err)
			os.Exit(1)
		}
	}

	current, err := readEnvFile(*envFile)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *envFile, err)
		os.Exit(1)
	}

	for k, v := range updates {
		current[k] = v
		fmt.Printf("  %s=%s\n", k, v)
	}

	if err := writeEnvFile(*envFile, current); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *envFile, err)
		os.Exit(1)
	}
	fmt.Printf("updated %s\n", *envFile)

	if !*noRestart {
		fmt.Printf("restarting %s...\n", *service)
		out, err := exec.Command("systemctl", "restart", *service).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "restart failed: %v\n%s\n", err, strings.TrimSpace(string(out)))
			os.Exit(1)
		}
		fmt.Println("done")
	}
}

func collectEnvUpdates(fs *flag.FlagSet, path, name *string, includeHidden, includeNetworkMounts, fresh *bool, keepIndexes *int, dbPath, interval, listen *string) map[string]string {
	updates := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "path":
			updates["INDEXER_PATH"] = *path
		case "name":
			updates["INDEXER_NAME"] = *name
		case "include-hidden":
			updates["INDEXER_INCLUDE_HIDDEN"] = strconv.FormatBool(*includeHidden)
		case "include-network-mounts":
			updates["INDEXER_INCLUDE_NETWORK_MOUNTS"] = strconv.FormatBool(*includeNetworkMounts)
		case "fresh":
			updates["INDEXER_FRESH"] = strconv.FormatBool(*fresh)
		case "keep-indexes":
			updates["INDEXER_KEEP_INDEXES"] = strconv.Itoa(*keepIndexes)
		case "db-path":
			updates["INDEXER_DB_PATH"] = *dbPath
		case "interval":
			updates["INDEXER_INTERVAL"] = *interval
		case "listen":
			if *listen == "" {
				updates["INDEXER_LISTEN_FLAG"] = ""
			} else {
				updates["INDEXER_LISTEN_FLAG"] = "--listen=" + *listen
			}
		}
	})
	return updates
}

// readEnvFile parses a systemd EnvironmentFile (KEY=VALUE lines, # comments ignored).
func readEnvFile(path string) (_ map[string]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return map[string]string{}, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	result := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		result[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return result, scanner.Err()
}

// writeEnvFile writes env vars in canonical key order to path.
func writeEnvFile(path string, env map[string]string) error {
	order := []string{
		"INDEXER_PATH", "INDEXER_NAME", "INDEXER_INCLUDE_HIDDEN",
		"INDEXER_INCLUDE_NETWORK_MOUNTS", "INDEXER_FRESH",
		"INDEXER_KEEP_INDEXES", "INDEXER_DB_PATH",
		"INDEXER_SOCKET", "INDEXER_INTERVAL", "INDEXER_LISTEN_FLAG",
	}
	inOrder := map[string]bool{}
	for _, k := range order {
		inOrder[k] = true
	}

	var sb strings.Builder
	for _, k := range order {
		if v, ok := env[k]; ok {
			fmt.Fprintf(&sb, "%s=%s\n", k, v)
		}
	}
	for k, v := range env {
		if !inOrder[k] {
			fmt.Fprintf(&sb, "%s=%s\n", k, v)
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
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

// queryDaemonPretty is like queryDaemon but pretty-prints JSON responses.
func queryDaemonPretty(socketPath, listenAddr, endpoint string) error {
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

	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Println(strings.TrimSpace(string(body)))
		return nil
	}
	fmt.Println(string(pretty))
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
