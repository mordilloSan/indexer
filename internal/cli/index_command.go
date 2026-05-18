package cli

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime/pprof"
	"strings"

	"github.com/mordilloSan/indexer/cmd"
	"github.com/mordilloSan/indexer/internal/configfile"
	"github.com/mordilloSan/indexer/internal/version"
	"github.com/mordilloSan/indexer/logging"
)

func runInternalIndexMode(args []string) {
	runIndexCommand("index-mode", args)
}

func runIndexCommand(flagSetName string, args []string) {
	fs := flag.NewFlagSet(flagSetName, flag.ExitOnError)
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file path")
	indexPath := fs.String("path", "", "Path to index")
	indexName := fs.String("name", "", "Index name")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network/external mounts such as NFS, SMB, and CIFS")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "Alias for --include-network-mounts")
	freshIndex := fs.Bool("fresh", true, "Fresh index mode")
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
	cpuProfile := fs.String("cpu-profile", "", "Write CPU profile to file")
	verbose := fs.Bool("verbose", false, "Enable verbose logging")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	hasConfigSource := flagWasSet(fs, "config-file") ||
		strings.TrimSpace(os.Getenv(configfile.EnvConfigFile)) != "" ||
		fileExists(*configPath)

	logging.Configure("indexer-index", *verbose)
	slog.Info("indexer starting", "version", version.String(), "mode", "index")

	if !flagWasSet(fs, "path") && !hasConfigSource {
		slog.Error("missing required flag", "flag", "--path")
		fs.Usage()
		os.Exit(1)
	}

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
	fileCfg, err = applyIndexFlagOverrides(fs, fileCfg, indexFlagValues{
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
	})
	if err != nil {
		slog.Error("invalid command-line config override", "err", err)
		os.Exit(1)
	}
	if flagWasSet(fs, "path") && !flagWasSet(fs, "name") {
		fileCfg.IndexName = configfile.DeriveIndexName(fileCfg.IndexPath)
	}

	if fileCfg.IndexPath == "" {
		slog.Error("missing required flag", "flag", "--path")
		fs.Usage()
		os.Exit(1)
	}

	dbOptions, err := configfile.DBOpenOptions(fileCfg)
	if err != nil {
		slog.Error("invalid database options", "err", err)
		os.Exit(1)
	}

	if err := withCPUProfile(*cpuProfile, func() error {
		return cmd.RunIndexMode(fileCfg.IndexName, fileCfg.IndexPath, fileCfg.IncludeHidden, fileCfg.IncludeNetworkMounts, fileCfg.FreshIndex, fileCfg.DBPath, fileCfg.KeepIndexes, dbOptions)
	}); err != nil {
		slog.Error("index failed", "err", err)
		os.Exit(1)
	}
}

type indexFlagValues struct {
	indexPath            *string
	indexName            *string
	includeHidden        *bool
	includeNetworkMounts *bool
	freshIndex           *bool
	keepIndexes          *int
	dbPath               *string
	dbBusyTimeout        *string
	dbJournalMode        *string
	dbSynchronous        *string
	dbAutoVacuum         *string
	dbMaxOpenConns       *int
	dbMaxIdleConns       *int
	dbConnMaxIdleTime    *string
}

func applyIndexFlagOverrides(fs *flag.FlagSet, cfg configfile.Config, values indexFlagValues) (configfile.Config, error) {
	var patch configfile.Patch
	if flagWasSet(fs, "path") {
		patch.IndexPath = values.indexPath
	}
	if flagWasSet(fs, "name") {
		patch.IndexName = values.indexName
	}
	if flagWasSet(fs, "include-hidden") {
		patch.IncludeHidden = values.includeHidden
	}
	if flagWasSet(fs, "include-network-mounts") || flagWasSet(fs, "include-external-mounts") {
		patch.IncludeNetworkMounts = values.includeNetworkMounts
	}
	if flagWasSet(fs, "fresh") {
		patch.FreshIndex = values.freshIndex
	}
	if flagWasSet(fs, "keep-indexes") {
		patch.KeepIndexes = values.keepIndexes
	}
	if flagWasSet(fs, "db-path") {
		patch.DBPath = values.dbPath
	}
	if flagWasSet(fs, "db-busy-timeout") {
		patch.DBBusyTimeout = values.dbBusyTimeout
	}
	if flagWasSet(fs, "db-journal-mode") {
		patch.DBJournalMode = values.dbJournalMode
	}
	if flagWasSet(fs, "db-synchronous") {
		patch.DBSynchronous = values.dbSynchronous
	}
	if flagWasSet(fs, "db-auto-vacuum") {
		patch.DBAutoVacuum = values.dbAutoVacuum
	}
	if flagWasSet(fs, "db-max-open-conns") {
		patch.DBMaxOpenConns = values.dbMaxOpenConns
	}
	if flagWasSet(fs, "db-max-idle-conns") {
		patch.DBMaxIdleConns = values.dbMaxIdleConns
	}
	if flagWasSet(fs, "db-conn-max-idle-time") {
		patch.DBConnMaxIdleTime = values.dbConnMaxIdleTime
	}
	return configfile.ApplyPatch(cfg, patch)
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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
			slog.Warn("failed to close cpu profile", "err", closeErr)
		}
	}()

	if err := pprof.StartCPUProfile(f); err != nil {
		return fmt.Errorf("start cpu profile: %w", err)
	}
	defer pprof.StopCPUProfile()

	slog.Info("CPU profiling enabled", "path", path)
	return run()
}

type indexOptions struct {
	FreshIndex           bool
	IncludeNetworkMounts bool
	KeepIndexes          int
}

func applyIndexEnvOverrides(fs *flag.FlagSet, opts *indexOptions) {
	if !flagWasSet(fs, "fresh") && strings.TrimSpace(os.Getenv("INDEXER_FRESH")) != "" {
		opts.FreshIndex = envBool("INDEXER_FRESH")
	}
	if !flagWasSet(fs, "include-network-mounts") && !flagWasSet(fs, "include-external-mounts") {
		opts.IncludeNetworkMounts = envBool("INDEXER_INCLUDE_NETWORK_MOUNTS") || envBool("INDEXER_INCLUDE_EXTERNAL_MOUNTS")
	}
	if !flagWasSet(fs, "keep-indexes") {
		opts.KeepIndexes = envInt("INDEXER_KEEP_INDEXES", opts.KeepIndexes)
	}
}
