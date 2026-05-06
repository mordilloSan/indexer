package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mordilloSan/indexer/internal/configfile"
)

func runConfigSet(args []string) {
	fs := flag.NewFlagSet("config set", flag.ExitOnError)
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	dryRun := fs.Bool("dry-run", false, "Print the resulting config file without writing or restarting")
	service := fs.String("service", defaultServiceName, "Systemd service name")

	path := fs.String("path", "", "Path to index")
	name := fs.String("name", "", "Index name")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network mounts")
	fresh := fs.Bool("fresh", false, "Fresh index mode")
	keepIndexes := fs.Int("keep-indexes", 0, "Records to keep after indexing (0=disabled)")
	dbPath := fs.String("db-path", "", "SQLite database path")
	dbBusyTimeout := fs.String("db-busy-timeout", "", "SQLite busy timeout")
	dbJournalMode := fs.String("db-journal-mode", "", "SQLite journal mode")
	dbSynchronous := fs.String("db-synchronous", "", "SQLite synchronous setting")
	dbAutoVacuum := fs.String("db-auto-vacuum", "", "SQLite auto_vacuum setting")
	dbMaxOpenConns := fs.Int("db-max-open-conns", 0, "SQLite max open connections")
	dbMaxIdleConns := fs.Int("db-max-idle-conns", 0, "SQLite max idle connections")
	dbConnMaxIdleTime := fs.String("db-conn-max-idle-time", "", "SQLite connection max idle time")
	socketPath := fs.String("socket-path", "", "Unix socket path (\"-\" to disable)")
	interval := fs.String("interval", "", "Auto-index interval (e.g. 1h, 30m, 0)")
	listen := fs.String("listen", "", "TCP listen address (e.g. :8080); empty to disable")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	patch := collectConfigPatch(fs, path, name, includeHidden, includeNetworkMounts, fresh, keepIndexes, dbPath, dbBusyTimeout, dbJournalMode, dbSynchronous, dbAutoVacuum, dbMaxOpenConns, dbMaxIdleConns, dbConnMaxIdleTime, socketPath, interval, listen)
	if patchIsEmpty(patch) {
		fmt.Fprintln(os.Stderr, "no settings specified; use flags like --interval=2h or --path=/data")
		fs.Usage()
		os.Exit(1)
	}

	current, err := configfile.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	next, err := configfile.ApplyPatch(current, patch)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	if *dryRun {
		if err := printConfigDryRun(*configPath, next); err != nil {
			fmt.Fprintf(os.Stderr, "format %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		return
	}

	if err := configfile.Save(*configPath, next); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	fmt.Printf("updated %s\n", *configPath)

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

func printConfigDryRun(configPath string, cfg configfile.Config) error {
	content, err := configfile.Format(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("dry run: would write %s:\n%s", configPath, content)
	return nil
}

func collectConfigPatch(fs *flag.FlagSet, path, name *string, includeHidden, includeNetworkMounts, fresh *bool, keepIndexes *int, dbPath, dbBusyTimeout, dbJournalMode, dbSynchronous, dbAutoVacuum *string, dbMaxOpenConns, dbMaxIdleConns *int, dbConnMaxIdleTime, socketPath, interval, listen *string) configfile.Patch {
	var patch configfile.Patch
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "path":
			patch.IndexPath = path
		case "name":
			patch.IndexName = name
		case "include-hidden":
			patch.IncludeHidden = includeHidden
		case "include-network-mounts":
			patch.IncludeNetworkMounts = includeNetworkMounts
		case "fresh":
			patch.FreshIndex = fresh
		case "keep-indexes":
			patch.KeepIndexes = keepIndexes
		case "db-path":
			patch.DBPath = dbPath
		case "db-busy-timeout":
			patch.DBBusyTimeout = dbBusyTimeout
		case "db-journal-mode":
			patch.DBJournalMode = dbJournalMode
		case "db-synchronous":
			patch.DBSynchronous = dbSynchronous
		case "db-auto-vacuum":
			patch.DBAutoVacuum = dbAutoVacuum
		case "db-max-open-conns":
			patch.DBMaxOpenConns = dbMaxOpenConns
		case "db-max-idle-conns":
			patch.DBMaxIdleConns = dbMaxIdleConns
		case "db-conn-max-idle-time":
			patch.DBConnMaxIdleTime = dbConnMaxIdleTime
		case "socket-path":
			patch.SocketPath = socketPath
		case "interval":
			patch.Interval = interval
		case "listen":
			patch.ListenAddr = listen
		}
	})
	return patch
}

func patchIsEmpty(patch configfile.Patch) bool {
	return patch.IndexPath == nil &&
		patch.IndexName == nil &&
		patch.IncludeHidden == nil &&
		patch.IncludeNetworkMounts == nil &&
		patch.FreshIndex == nil &&
		patch.KeepIndexes == nil &&
		patch.DBPath == nil &&
		patch.DBBusyTimeout == nil &&
		patch.DBJournalMode == nil &&
		patch.DBSynchronous == nil &&
		patch.DBAutoVacuum == nil &&
		patch.DBMaxOpenConns == nil &&
		patch.DBMaxIdleConns == nil &&
		patch.DBConnMaxIdleTime == nil &&
		patch.SocketPath == nil &&
		patch.ListenAddr == nil &&
		patch.Interval == nil
}
