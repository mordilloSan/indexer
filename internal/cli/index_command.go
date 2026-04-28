package cli

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"runtime/pprof"
	"strings"

	"github.com/mordilloSan/indexer/cmd"
	"github.com/mordilloSan/indexer/internal/version"
)

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
	indexOpts := indexOptions{
		FreshIndex:           *freshIndex,
		IncludeNetworkMounts: *includeNetworkMounts,
		KeepIndexes:          *keepIndexes,
	}
	applyIndexEnvOverrides(fs, &indexOpts)

	if err := withCPUProfile(*cpuProfile, func() error {
		return cmd.RunIndexMode(nameVal, *indexPath, *includeHidden, indexOpts.IncludeNetworkMounts, indexOpts.FreshIndex, dbVal, indexOpts.KeepIndexes)
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
