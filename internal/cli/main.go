package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mordilloSan/indexer/cmd"
	"github.com/mordilloSan/indexer/internal/version"
)

const (
	defaultEnvFile     = "/etc/default/indexer"
	defaultServiceName = "indexer"
)

const usageText = `Usage: indexer <command> [flags]

Commands:
  daemon    Start the indexer daemon
  index     Index one folder and exit
  status    Query status from a running daemon
  config    Show configuration of a running daemon
  setup     Interactive systemd configuration wizard
  version   Print version information

Run 'indexer <command> --help' for details on a specific command.
`

func Main(args []string) {
	// Internal: handle --index-mode for subprocess spawning (not shown in help)
	if len(args) > 0 && args[0] == "--index-mode" {
		runInternalIndexMode(args[1:])
		return
	}

	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(1)
	}

	switch args[0] {
	case "daemon":
		runDaemon(args[1:])
	case "index":
		runIndex(args[1:])
	case "status":
		runStatus(args[1:])
	case "config":
		runConfig(args[1:])
	case "setup":
		runSetup(args[1:])
	case "version":
		fmt.Println(version.String())
	case "--help", "-h", "help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", args[0])
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
	indexOpts := indexOptions{
		FreshIndex:           *freshIndex,
		IncludeNetworkMounts: *includeNetworkMounts,
		KeepIndexes:          *keepIndexes,
	}
	applyIndexEnvOverrides(fs, &indexOpts)

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
		IncludeNetworkMounts: indexOpts.IncludeNetworkMounts,
		FreshIndex:           indexOpts.FreshIndex,
		KeepIndexes:          indexOpts.KeepIndexes,
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
