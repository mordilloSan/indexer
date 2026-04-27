package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	case "setup":
		runSetup(os.Args[2:])
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
	fs := flag.NewFlagSet("config set", flag.ExitOnError)
	envFile := fs.String("env-file", defaultEnvFile, "Environment file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	service := fs.String("service", defaultServiceName, "Systemd service name")

	path := fs.String("path", "", "Path to index")
	name := fs.String("name", "", "Index name")
	includeHidden := fs.Bool("include-hidden", false, "Include hidden files")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "Include network mounts")
	fresh := fs.Bool("fresh", false, "Fresh index mode")
	keepIndexes := fs.Int("keep-indexes", 0, "Records to keep after indexing (0=disabled)")
	dbPath := fs.String("db-path", "", "SQLite database path")
	socketPath := fs.String("socket-path", "", "Unix socket path (\"-\" to disable)")
	interval := fs.String("interval", "", "Auto-index interval (e.g. 1h, 30m, 0)")
	listen := fs.String("listen", "", "TCP listen address (e.g. :8080); empty to disable")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	updates := collectEnvUpdates(fs, path, name, includeHidden, includeNetworkMounts, fresh, keepIndexes, dbPath, socketPath, interval, listen)

	if len(updates) == 0 {
		fmt.Fprintln(os.Stderr, "no settings specified; use flags like --interval=2h or --path=/data")
		fs.Usage()
		os.Exit(1)
	}

	if err := validateConfigUpdates(updates); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	current, err := readEnvFile(*envFile)
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *envFile, err)
		os.Exit(1)
	}

	for k, v := range updates {
		current[k] = v
		line, err := formatEnvAssignment(k, v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid setting %s: %v\n", k, err)
			os.Exit(1)
		}
		fmt.Printf("  %s\n", line)
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

func collectEnvUpdates(fs *flag.FlagSet, path, name *string, includeHidden, includeNetworkMounts, fresh *bool, keepIndexes *int, dbPath, socketPath, interval, listen *string) map[string]string {
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
		case "socket-path":
			updates["INDEXER_SOCKET"] = *socketPath
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

func validateConfigUpdates(updates map[string]string) error {
	for _, key := range []string{"INDEXER_PATH", "INDEXER_NAME", "INDEXER_DB_PATH"} {
		if value, ok := updates[key]; ok && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s cannot be empty", key)
		}
	}
	if value, ok := updates["INDEXER_SOCKET"]; ok && strings.TrimSpace(value) == "" {
		return fmt.Errorf("INDEXER_SOCKET cannot be empty; use - to disable the Unix socket")
	}
	if value, ok := updates["INDEXER_INTERVAL"]; ok {
		if _, err := parseInterval(value); err != nil {
			return fmt.Errorf("invalid interval %q: %w", value, err)
		}
	}
	if value, ok := updates["INDEXER_KEEP_INDEXES"]; ok {
		keep, err := strconv.Atoi(value)
		if err != nil || keep < 0 {
			return fmt.Errorf("invalid keep-indexes %q: must be a non-negative integer", value)
		}
	}
	return nil
}

type setupConfig struct {
	IndexPath            string
	IndexName            string
	IncludeHidden        bool
	IncludeNetworkMounts bool
	FreshIndex           bool
	KeepIndexes          int
	DBPath               string
	SocketPath           string
	Interval             string
	ListenAddr           string
	ListenFlag           string
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	envFile := fs.String("env-file", defaultEnvFile, "Environment file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	service := fs.String("service", defaultServiceName, "Systemd service name")
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path for reading current daemon config")
	listenAddr := fs.String("listen", "", "TCP address for reading current daemon config (e.g. :8080)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	cfg, source, env, err := loadSetupConfig(*envFile, *socketPath, *listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	fmt.Println("Indexer Setup")
	fmt.Println("-------------")
	fmt.Printf("Using current values from %s.\n", source)
	fmt.Println("Press Enter to keep the value in brackets.")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	cfg, apply, err := runSetupWizard(reader, os.Stdout, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if !apply {
		fmt.Println("canceled")
		return
	}

	maps.Copy(env, setupConfigToEnv(cfg))
	if err := writeEnvFile(*envFile, env); err != nil {
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

func loadSetupConfig(envFile, socketPath, listenAddr string) (setupConfig, string, map[string]string, error) {
	cfg := defaultSetupConfig()
	source := "built-in defaults"
	env := map[string]string{}

	fileEnv, err := readEnvFile(envFile)
	if err == nil {
		env = fileEnv
		cfg = setupConfigFromEnv(fileEnv)
		source = envFile
	} else if !os.IsNotExist(err) {
		return cfg, "", nil, fmt.Errorf("read %s: %w", envFile, err)
	}

	daemonCfg, err := queryDaemonSetupConfig(socketPath, listenAddr)
	if err == nil {
		return daemonCfg, "running daemon", env, nil
	}
	if source == "built-in defaults" {
		source = fmt.Sprintf("built-in defaults (daemon unavailable: %v)", err)
	} else {
		source = fmt.Sprintf("%s (daemon unavailable: %v)", source, err)
	}
	return cfg, source, env, nil
}

func defaultSetupConfig() setupConfig {
	return setupConfig{
		IndexPath:            "/",
		IndexName:            "root",
		IncludeHidden:        true,
		IncludeNetworkMounts: false,
		FreshIndex:           true,
		KeepIndexes:          0,
		DBPath:               "/tmp/indexer.db",
		SocketPath:           "/var/run/indexer.sock",
		Interval:             "1h",
		ListenAddr:           "",
	}
}

func setupConfigFromEnv(env map[string]string) setupConfig {
	cfg := defaultSetupConfig()
	if v, ok := env["INDEXER_PATH"]; ok {
		cfg.IndexPath = v
	}
	if v, ok := env["INDEXER_NAME"]; ok {
		cfg.IndexName = v
	}
	if v, ok := env["INDEXER_INCLUDE_HIDDEN"]; ok {
		cfg.IncludeHidden = parseBoolWithFallback(v, cfg.IncludeHidden)
	}
	if v, ok := env["INDEXER_INCLUDE_NETWORK_MOUNTS"]; ok {
		cfg.IncludeNetworkMounts = parseBoolWithFallback(v, cfg.IncludeNetworkMounts)
	}
	if v, ok := env["INDEXER_FRESH"]; ok {
		cfg.FreshIndex = parseBoolWithFallback(v, cfg.FreshIndex)
	}
	if v, ok := env["INDEXER_KEEP_INDEXES"]; ok {
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && parsed >= 0 {
			cfg.KeepIndexes = parsed
		}
	}
	if v, ok := env["INDEXER_DB_PATH"]; ok {
		cfg.DBPath = v
	}
	if v, ok := env["INDEXER_SOCKET"]; ok {
		cfg.SocketPath = v
	}
	if v, ok := env["INDEXER_INTERVAL"]; ok {
		cfg.Interval = v
	}
	if v, ok := env["INDEXER_LISTEN_FLAG"]; ok {
		cfg.ListenAddr, cfg.ListenFlag = splitListenFlag(v)
	}
	return cfg
}

func setupConfigToEnv(cfg setupConfig) map[string]string {
	listenFlag := strings.TrimSpace(cfg.ListenFlag)
	if strings.TrimSpace(cfg.ListenAddr) != "" {
		listenFlag = "--listen=" + strings.TrimSpace(cfg.ListenAddr)
	}
	return map[string]string{
		"INDEXER_PATH":                   cfg.IndexPath,
		"INDEXER_NAME":                   cfg.IndexName,
		"INDEXER_INCLUDE_HIDDEN":         strconv.FormatBool(cfg.IncludeHidden),
		"INDEXER_INCLUDE_NETWORK_MOUNTS": strconv.FormatBool(cfg.IncludeNetworkMounts),
		"INDEXER_FRESH":                  strconv.FormatBool(cfg.FreshIndex),
		"INDEXER_KEEP_INDEXES":           strconv.Itoa(cfg.KeepIndexes),
		"INDEXER_DB_PATH":                cfg.DBPath,
		"INDEXER_SOCKET":                 cfg.SocketPath,
		"INDEXER_INTERVAL":               cfg.Interval,
		"INDEXER_LISTEN_FLAG":            listenFlag,
	}
}

func queryDaemonSetupConfig(socketPath, listenAddr string) (setupConfig, error) {
	body, err := fetchDaemonBody(socketPath, listenAddr, "/config")
	if err != nil {
		return setupConfig{}, err
	}

	var resp struct {
		IndexName            string `json:"index_name"`
		IndexPath            string `json:"index_path"`
		IncludeHidden        bool   `json:"include_hidden"`
		IncludeNetworkMounts bool   `json:"include_network_mounts"`
		FreshIndex           bool   `json:"fresh_index"`
		KeepIndexes          int    `json:"keep_indexes"`
		DBPath               string `json:"db_path"`
		SocketPath           string `json:"socket_path"`
		ListenAddr           string `json:"listen_addr"`
		Interval             string `json:"interval"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return setupConfig{}, fmt.Errorf("parse daemon config: %w", err)
	}

	socketPath = resp.SocketPath
	if socketPath == "" {
		socketPath = "-"
	}
	interval := resp.Interval
	if interval == "0s" {
		interval = "0"
	}
	return setupConfig{
		IndexPath:            resp.IndexPath,
		IndexName:            resp.IndexName,
		IncludeHidden:        resp.IncludeHidden,
		IncludeNetworkMounts: resp.IncludeNetworkMounts,
		FreshIndex:           resp.FreshIndex,
		KeepIndexes:          resp.KeepIndexes,
		DBPath:               resp.DBPath,
		SocketPath:           socketPath,
		Interval:             interval,
		ListenAddr:           resp.ListenAddr,
		ListenFlag:           "",
	}, nil
}

func splitListenFlag(raw string) (listenAddr, extraFlag string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if after, ok := strings.CutPrefix(raw, "--listen="); ok {
		return strings.TrimSpace(after), ""
	}
	parts := strings.Fields(raw)
	if len(parts) == 2 && parts[0] == "--listen" {
		return parts[1], ""
	}
	return "", raw
}

func runSetupWizard(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, bool, error) {
	var err error
	for i := 1; i <= 10; i++ {
		cfg, err = promptSetupField(reader, w, cfg, i)
		if err != nil {
			return cfg, false, err
		}
	}

	for {
		if err := printSetupSummary(w, cfg); err != nil {
			return cfg, false, err
		}
		choice, err := promptLine(reader, w, "Apply changes, edit field number, or cancel [apply]: ")
		if err != nil {
			return cfg, false, err
		}
		choice = strings.ToLower(strings.TrimSpace(choice))
		switch choice {
		case "", "a", "apply", "y", "yes":
			return cfg, true, nil
		case "c", "cancel", "q", "quit", "n", "no":
			return cfg, false, nil
		}
		n, err := strconv.Atoi(choice)
		if err != nil || n < 1 || n > 10 {
			_, _ = fmt.Fprintln(w, "Enter a field number from 1 to 10, apply, or cancel.")
			continue
		}
		cfg, err = promptSetupField(reader, w, cfg, n)
		if err != nil {
			return cfg, false, err
		}
	}
}

func printSetupSummary(w io.Writer, cfg setupConfig) error {
	fields := setupSummaryFields(cfg)

	var sb strings.Builder
	sb.WriteString("\nReview\n------\n")
	for _, field := range fields {
		fmt.Fprintf(&sb, " %2d. %s: %s\n", field.Number, field.Label, field.Value)
	}
	if cfg.ListenFlag != "" && strings.TrimSpace(cfg.ListenAddr) == "" {
		fmt.Fprintf(&sb, "     Preserved INDEXER_LISTEN_FLAG: %s\n", cfg.ListenFlag)
	}
	sb.WriteByte('\n')
	_, err := io.WriteString(w, sb.String())
	return err
}

type setupSummaryField struct {
	Number int
	Label  string
	Value  string
}

func setupSummaryFields(cfg setupConfig) []setupSummaryField {
	return []setupSummaryField{
		{1, "Index path", displayOptional(cfg.IndexPath, "unset")},
		{2, "Index name", displayOptional(cfg.IndexName, "unset")},
		{3, "Include hidden files", strconv.FormatBool(cfg.IncludeHidden)},
		{4, "Include network mounts", strconv.FormatBool(cfg.IncludeNetworkMounts)},
		{5, "Fresh index", strconv.FormatBool(cfg.FreshIndex)},
		{6, "Keep indexes", strconv.Itoa(cfg.KeepIndexes)},
		{7, "Database path", displayOptional(cfg.DBPath, "unset")},
		{8, "Unix socket path", displayOptional(cfg.SocketPath, "disabled")},
		{9, "Auto-index interval", displayOptional(cfg.Interval, "disabled")},
		{10, "TCP listen address", displayOptional(cfg.ListenAddr, "disabled")},
	}
}

func promptSetupField(reader *bufio.Reader, w io.Writer, cfg setupConfig, field int) (setupConfig, error) {
	var err error
	switch field {
	case 1:
		cfg.IndexPath, err = promptRequiredString(reader, w, "Index path", cfg.IndexPath)
	case 2:
		cfg.IndexName, err = promptRequiredString(reader, w, "Index name", cfg.IndexName)
	case 3:
		cfg.IncludeHidden, err = promptBool(reader, w, "Include hidden files", cfg.IncludeHidden)
	case 4:
		cfg.IncludeNetworkMounts, err = promptBool(reader, w, "Include network mounts", cfg.IncludeNetworkMounts)
	case 5:
		cfg.FreshIndex, err = promptBool(reader, w, "Fresh index", cfg.FreshIndex)
	case 6:
		cfg.KeepIndexes, err = promptNonNegativeInt(reader, w, "Keep indexes", cfg.KeepIndexes)
	case 7:
		cfg.DBPath, err = promptRequiredString(reader, w, "Database path", cfg.DBPath)
	case 8:
		cfg.SocketPath, err = promptOptionalString(reader, w, "Unix socket path (type none to disable)", cfg.SocketPath, "-")
	case 9:
		cfg.Interval, err = promptInterval(reader, w, "Auto-index interval", cfg.Interval)
	case 10:
		cfg.ListenAddr, cfg.ListenFlag, err = promptListenAddr(reader, w, cfg.ListenAddr, cfg.ListenFlag)
	}
	return cfg, err
}

func promptRequiredString(reader *bufio.Reader, w io.Writer, label, current string) (string, error) {
	for {
		value, err := promptString(reader, w, label, current)
		if err != nil {
			return current, err
		}
		if strings.TrimSpace(value) == "" {
			_, _ = fmt.Fprintln(w, "Value cannot be empty.")
			continue
		}
		return strings.TrimSpace(value), nil
	}
}

func promptOptionalString(reader *bufio.Reader, w io.Writer, label, current, disabledValue string) (string, error) {
	value, err := promptString(reader, w, label, current)
	if err != nil {
		return current, err
	}
	if strings.EqualFold(strings.TrimSpace(value), "none") {
		return disabledValue, nil
	}
	return strings.TrimSpace(value), nil
}

func promptInterval(reader *bufio.Reader, w io.Writer, label, current string) (string, error) {
	for {
		value, err := promptOptionalString(reader, w, label+" (0 disables)", current, "0")
		if err != nil {
			return current, err
		}
		if _, err := parseInterval(value); err != nil {
			_, _ = fmt.Fprintf(w, "Invalid duration %q. Use values like 30m, 1h, 6h, or 0.\n", value)
			continue
		}
		return value, nil
	}
}

func promptListenAddr(reader *bufio.Reader, w io.Writer, currentAddr, currentFlag string) (string, string, error) {
	if currentFlag != "" && strings.TrimSpace(currentAddr) == "" {
		_, _ = fmt.Fprintf(w, "Existing INDEXER_LISTEN_FLAG %q will be preserved. Enter a listen address to replace it, or type none to clear it.\n", currentFlag)
	}
	value, err := promptLine(reader, w, fmt.Sprintf("TCP listen address (type none to disable) [%s]: ", displayOptional(currentAddr, "disabled")))
	if err != nil {
		return currentAddr, currentFlag, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return currentAddr, currentFlag, nil
	}
	if strings.EqualFold(value, "none") {
		return "", "", nil
	}
	return value, "", nil
}

func promptNonNegativeInt(reader *bufio.Reader, w io.Writer, label string, current int) (int, error) {
	for {
		value, err := promptString(reader, w, label, strconv.Itoa(current))
		if err != nil {
			return current, err
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || parsed < 0 {
			_, _ = fmt.Fprintln(w, "Enter a non-negative integer.")
			continue
		}
		return parsed, nil
	}
}

func promptBool(reader *bufio.Reader, w io.Writer, label string, current bool) (bool, error) {
	suffix := "y/N"
	if current {
		suffix = "Y/n"
	}
	for {
		value, err := promptLine(reader, w, fmt.Sprintf("%s [%s]: ", label, suffix))
		if err != nil {
			return current, err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return current, nil
		}
		if parsed, ok := parseBool(value); ok {
			return parsed, nil
		}
		_, _ = fmt.Fprintln(w, "Enter yes or no.")
	}
}

func promptString(reader *bufio.Reader, w io.Writer, label, current string) (string, error) {
	value, err := promptLine(reader, w, fmt.Sprintf("%s [%s]: ", label, displayOptional(current, "disabled")))
	if err != nil {
		return current, err
	}
	if value == "" {
		return current, nil
	}
	return value, nil
}

func promptLine(reader *bufio.Reader, w io.Writer, prompt string) (string, error) {
	_, _ = fmt.Fprint(w, prompt)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if err == io.EOF && line == "" {
		return "", fmt.Errorf("input closed")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func parseBoolWithFallback(raw string, fallback bool) bool {
	if parsed, ok := parseBool(raw); ok {
		return parsed
	}
	return fallback
}

func parseBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, true
	case "0", "f", "false", "n", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func displayOptional(value, fallback string) string {
	if strings.TrimSpace(value) == "" || value == "-" && fallback == "disabled" {
		return fallback
	}
	return value
}

// readEnvFile parses the subset of systemd EnvironmentFile syntax used by the
// indexer service: one KEY=VALUE assignment per line, with quoted values when
// whitespace or shell-significant characters are needed.
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
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok, err := parseEnvLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if !ok {
			continue
		}
		result[key] = value
	}
	return result, scanner.Err()
}

// writeEnvFile writes env vars in canonical key order to path.
func writeEnvFile(path string, env map[string]string) error {
	content, err := formatEnvFile(env)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func formatEnvFile(env map[string]string) (string, error) {
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
			line, err := formatEnvAssignment(k, v)
			if err != nil {
				return "", err
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	var extraKeys []string
	for k := range env {
		if !inOrder[k] {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		line, err := formatEnvAssignment(k, env[k])
		if err != nil {
			return "", err
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

func parseEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
		return "", "", false, nil
	}

	keyPart, valuePart, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("expected KEY=VALUE assignment")
	}
	key = strings.TrimSpace(keyPart)
	if !isValidEnvKey(key) {
		return "", "", false, fmt.Errorf("invalid environment key %q", key)
	}
	value, err = parseEnvValue(strings.TrimSpace(valuePart))
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

func parseEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		return parseSingleQuotedEnvValue(raw)
	case '"':
		return parseDoubleQuotedEnvValue(raw)
	default:
		return parseUnquotedEnvValue(raw)
	}
}

func parseSingleQuotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	for i := 1; i < len(raw); i++ {
		if raw[i] == '\'' {
			if err := validateEnvTrailingText(raw[i+1:]); err != nil {
				return "", err
			}
			return sb.String(), nil
		}
		sb.WriteByte(raw[i])
	}
	return "", fmt.Errorf("unterminated single-quoted value")
}

func parseDoubleQuotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	escaped := false
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			switch ch {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(ch)
			}
			escaped = false
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '"':
			if err := validateEnvTrailingText(raw[i+1:]); err != nil {
				return "", err
			}
			return sb.String(), nil
		default:
			sb.WriteByte(ch)
		}
	}
	if escaped {
		return "", fmt.Errorf("unterminated escape in double-quoted value")
	}
	return "", fmt.Errorf("unterminated double-quoted value")
}

func parseUnquotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	escaped := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			sb.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		sb.WriteByte(ch)
	}
	if escaped {
		return "", fmt.Errorf("unterminated escape in unquoted value")
	}
	return strings.TrimSpace(sb.String()), nil
}

func validateEnvTrailingText(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, ";") {
		return nil
	}
	return fmt.Errorf("unexpected text after quoted value")
}

func formatEnvAssignment(key, value string) (string, error) {
	if !isValidEnvKey(key) {
		return "", fmt.Errorf("invalid environment key %q", key)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s contains a newline or NUL byte", key)
	}
	return key + "=" + quoteEnvValue(value), nil
}

func quoteEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if isSafeEnvValue(value) {
		return value
	}

	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\', '"':
			sb.WriteByte('\\')
			sb.WriteByte(value[i])
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteByte(value[i])
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func isSafeEnvValue(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			continue
		}
		switch ch {
		case '/', '.', '_', '-', ':', '@', '%', '+', '=', ',':
			continue
		default:
			return false
		}
	}
	return true
}

func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		ch := key[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_' {
			continue
		}
		if i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return false
	}
	return true
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
	body, err := fetchDaemonBody(socketPath, listenAddr, endpoint)
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(string(body)))
	return nil
}

// queryDaemonPretty is like queryDaemon but pretty-prints JSON responses.
func queryDaemonPretty(socketPath, listenAddr, endpoint string) error {
	body, err := fetchDaemonBody(socketPath, listenAddr, endpoint)
	if err != nil {
		return err
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

func fetchDaemonBody(socketPath, listenAddr, endpoint string) ([]byte, error) {
	client, url, err := buildClient(socketPath, listenAddr, endpoint)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", url, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close response body: %v\n", closeErr)
		}
	}()

	body, err := ioReadAllLimit(resp.Body, 2<<20)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
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
