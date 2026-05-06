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
	"github.com/mordilloSan/indexer/internal/configfile"
	"github.com/mordilloSan/indexer/internal/version"
	"github.com/mordilloSan/indexer/logging"
)

const (
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
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file path")
	indexPath := fs.String("path", "", "Path to index")
	indexName := fs.String("name", "", "Name for this index (defaults to sanitized path)")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files and directories")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network/external mounts such as NFS, SMB, and CIFS")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "Alias for --include-network-mounts")
	freshIndex := fs.Bool("fresh", true, "Clear database before each full index (no previous results kept)")
	keepIndexes := fs.Int("keep-indexes", 0, "Most recent index records to keep after indexing (0 disables automatic pruning)")
	dbPath := fs.String("db-path", "", "SQLite database path")
	defaultCfg := configfile.Defaults()
	dbBusyTimeout := fs.String("db-busy-timeout", defaultCfg.DBBusyTimeout, "SQLite busy timeout")
	dbJournalMode := fs.String("db-journal-mode", defaultCfg.DBJournalMode, "SQLite journal mode")
	dbSynchronous := fs.String("db-synchronous", defaultCfg.DBSynchronous, "SQLite synchronous setting")
	dbAutoVacuum := fs.String("db-auto-vacuum", defaultCfg.DBAutoVacuum, "SQLite auto_vacuum setting")
	dbMaxOpenConns := fs.Int("db-max-open-conns", defaultCfg.DBMaxOpenConns, "SQLite max open connections")
	dbMaxIdleConns := fs.Int("db-max-idle-conns", defaultCfg.DBMaxIdleConns, "SQLite max idle connections")
	dbConnMaxIdleTime := fs.String("db-conn-max-idle-time", defaultCfg.DBConnMaxIdleTime, "SQLite connection max idle time")
	socketPath := fs.String("socket-path", "", "Unix socket path (\"-\" to disable)")
	listenAddr := fs.String("listen", "", "Optional TCP address (e.g., :8080)")
	indexInterval := fs.String("interval", "", "Auto-index interval (Go duration like 6h, 30m); 0 disables")
	verbose := fs.Bool("verbose", false, "Enable verbose logging")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	logging.Configure("indexer", *verbose)
	slog.Info("indexer starting", "version", version.String(), "mode", "daemon")

	fileCfg, err := configfile.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "config_file", *configPath, "err", err)
		os.Exit(1)
	}
	fileCfg, err = configfile.ApplyEnvOverrides(fileCfg, os.LookupEnv)
	if err != nil {
		slog.Error("invalid environment config override", "err", err)
		os.Exit(1)
	}
	fileCfg, err = applyDaemonFlagOverrides(fs, fileCfg, daemonFlagValues{
		indexPath:            indexPath,
		indexName:            indexName,
		includeHidden:        includeHidden,
		includeNetworkMounts: includeNetworkMounts,
		freshIndex:           freshIndex,
		keepIndexes:          keepIndexes,
		dbPath:               dbPath,
		dbBusyTimeout:        dbBusyTimeout,
		dbJournalMode:        dbJournalMode,
		dbSynchronous:        dbSynchronous,
		dbAutoVacuum:         dbAutoVacuum,
		dbMaxOpenConns:       dbMaxOpenConns,
		dbMaxIdleConns:       dbMaxIdleConns,
		dbConnMaxIdleTime:    dbConnMaxIdleTime,
		socketPath:           socketPath,
		listenAddr:           listenAddr,
		interval:             indexInterval,
	})
	if err != nil {
		slog.Error("invalid command-line config override", "err", err)
		os.Exit(1)
	}

	cfg, err := cmd.DaemonConfigFromConfig(fileCfg, *configPath)
	if err != nil {
		slog.Error("invalid daemon config", "err", err)
		os.Exit(1)
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
		"config_file", cfg.ConfigPath,
		"path", cfg.IndexPath,
		"name", cfg.IndexName,
		"db", cfg.DBPath,
		"db_journal_mode", cfg.DBOptions.JournalMode,
		"db_synchronous", cfg.DBOptions.Synchronous,
		"socket", cfg.SocketPath,
		"listen", listenDisplay,
		"include_hidden", cfg.IncludeHidden,
		"include_network_mounts", cfg.IncludeNetworkMounts,
		"keep_indexes", cfg.KeepIndexes,
		"interval", cfg.Interval,
	)

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
