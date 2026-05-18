package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mordilloSan/indexer/internal/configfile"
)

type setupConfig struct {
	configfile.Config
	ListenFlag string
}

type setupField struct {
	Label  string
	Value  func(setupConfig) string
	Prompt func(*bufio.Reader, io.Writer, setupConfig) (setupConfig, error)
}

var setupFields = []setupField{
	{
		Label: "Index path",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.IndexPath, "unset")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.IndexPath, err = promptRequiredString(reader, w, "Index path", cfg.IndexPath)
			return cfg, err
		},
	},
	{
		Label: "Index name",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.IndexName, "unset")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.IndexName, err = promptRequiredString(reader, w, "Index name", cfg.IndexName)
			return cfg, err
		},
	},
	{
		Label: "Include hidden files",
		Value: func(cfg setupConfig) string {
			return strconv.FormatBool(cfg.IncludeHidden)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.IncludeHidden, err = promptBool(reader, w, "Include hidden files", cfg.IncludeHidden)
			return cfg, err
		},
	},
	{
		Label: "Include network mounts",
		Value: func(cfg setupConfig) string {
			return strconv.FormatBool(cfg.IncludeNetworkMounts)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.IncludeNetworkMounts, err = promptBool(reader, w, "Include network mounts", cfg.IncludeNetworkMounts)
			return cfg, err
		},
	},
	{
		Label: "Fresh index",
		Value: func(cfg setupConfig) string {
			return strconv.FormatBool(cfg.FreshIndex)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.FreshIndex, err = promptBool(reader, w, "Fresh index", cfg.FreshIndex)
			return cfg, err
		},
	},
	{
		Label: "Keep indexes",
		Value: func(cfg setupConfig) string {
			return strconv.Itoa(cfg.KeepIndexes)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.KeepIndexes, err = promptNonNegativeInt(reader, w, "Keep indexes", cfg.KeepIndexes)
			return cfg, err
		},
	},
	{
		Label: "Database path",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.DBPath, "unset")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBPath, err = promptRequiredString(reader, w, "Database path", cfg.DBPath)
			return cfg, err
		},
	},
	{
		Label: "SQLite busy timeout",
		Value: func(cfg setupConfig) string {
			return cfg.DBBusyTimeout
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBBusyTimeout, err = promptInterval(reader, w, "SQLite busy timeout", cfg.DBBusyTimeout)
			return cfg, err
		},
	},
	{
		Label: "SQLite journal mode",
		Value: func(cfg setupConfig) string {
			return cfg.DBJournalMode
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBJournalMode, err = promptRequiredString(reader, w, "SQLite journal mode", cfg.DBJournalMode)
			return cfg, err
		},
	},
	{
		Label: "SQLite synchronous",
		Value: func(cfg setupConfig) string {
			return cfg.DBSynchronous
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBSynchronous, err = promptRequiredString(reader, w, "SQLite synchronous", cfg.DBSynchronous)
			return cfg, err
		},
	},
	{
		Label: "SQLite auto vacuum",
		Value: func(cfg setupConfig) string {
			return cfg.DBAutoVacuum
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBAutoVacuum, err = promptRequiredString(reader, w, "SQLite auto vacuum", cfg.DBAutoVacuum)
			return cfg, err
		},
	},
	{
		Label: "SQLite max open connections",
		Value: func(cfg setupConfig) string {
			return strconv.Itoa(cfg.DBMaxOpenConns)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBMaxOpenConns, err = promptNonNegativeInt(reader, w, "SQLite max open connections", cfg.DBMaxOpenConns)
			return cfg, err
		},
	},
	{
		Label: "SQLite max idle connections",
		Value: func(cfg setupConfig) string {
			return strconv.Itoa(cfg.DBMaxIdleConns)
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBMaxIdleConns, err = promptNonNegativeInt(reader, w, "SQLite max idle connections", cfg.DBMaxIdleConns)
			return cfg, err
		},
	},
	{
		Label: "SQLite connection max idle time",
		Value: func(cfg setupConfig) string {
			return cfg.DBConnMaxIdleTime
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.DBConnMaxIdleTime, err = promptInterval(reader, w, "SQLite connection max idle time", cfg.DBConnMaxIdleTime)
			return cfg, err
		},
	},
	{
		Label: "Unix socket path",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.SocketPath, "disabled")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.SocketPath, err = promptOptionalString(reader, w, "Unix socket path (type none to disable)", cfg.SocketPath, "-")
			return cfg, err
		},
	},
	{
		Label: "Auto-index interval",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.Interval, "disabled")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.Interval, err = promptInterval(reader, w, "Auto-index interval", cfg.Interval)
			return cfg, err
		},
	},
	{
		Label: "TCP listen address",
		Value: func(cfg setupConfig) string {
			return displayOptional(cfg.ListenAddr, "disabled")
		},
		Prompt: func(reader *bufio.Reader, w io.Writer, cfg setupConfig) (setupConfig, error) {
			var err error
			cfg.ListenAddr, cfg.ListenFlag, err = promptListenAddr(reader, w, cfg.ListenAddr, cfg.ListenFlag)
			return cfg, err
		},
	},
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	dryRun := fs.Bool("dry-run", false, "Print the resulting config file without writing or restarting")
	service := fs.String("service", defaultServiceName, "Systemd service name")
	socketUnit := fs.String("socket-unit", defaultSocketUnit, "Systemd socket unit name")
	timer := fs.String("timer", defaultTimerUnit, "Systemd index timer unit name")
	runtimeConfig := fs.Bool("runtime", false, "Read current values from the running daemon")
	socketPath := fs.String("socket-path", "/var/run/indexer.sock", "Unix socket path for reading current daemon config")
	listenAddr := fs.String("listen", "", "TCP address for reading current daemon config (e.g. :8080)")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	cfg, source, err := loadSetupConfig(*configPath, *socketPath, *listenAddr, *runtimeConfig)
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

	fileCfg, err := setupConfigToFileConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	if *dryRun {
		if err := printConfigDryRun(*configPath, fileCfg); err != nil {
			fmt.Fprintf(os.Stderr, "format %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		return
	}

	if err := configfile.Save(*configPath, fileCfg); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	fmt.Printf("updated %s\n", *configPath)

	if !*noRestart {
		patch := configPatchForSystemdApply(fileCfg)
		if err := applySystemdConfigChanges(patch, fileCfg, *service, *socketUnit, *timer); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	}
}

func configPatchForSystemdApply(cfg configfile.Config) configfile.Patch {
	return configfile.Patch{
		SocketPath: &cfg.SocketPath,
		Interval:   &cfg.Interval,
	}
}

func loadSetupConfig(configPath, socketPath, listenAddr string, runtimeConfig bool) (setupConfig, string, error) {
	cfg := defaultSetupConfig()
	source := "built-in defaults"

	fileCfg, err := configfile.Load(configPath)
	if err != nil {
		return cfg, "", fmt.Errorf("read %s: %w", configPath, err)
	}
	if _, err := os.Stat(configPath); err == nil {
		cfg = setupConfigFromFileConfig(fileCfg)
		source = configPath
	} else if !os.IsNotExist(err) {
		return cfg, "", fmt.Errorf("stat %s: %w", configPath, err)
	}

	if runtimeConfig {
		daemonCfg, err := queryDaemonSetupConfig(socketPath, listenAddr)
		if err == nil {
			return daemonCfg, "running daemon", nil
		}
		if source == "built-in defaults" {
			source = "built-in defaults (daemon not reachable)"
		} else {
			source = fmt.Sprintf("%s (daemon not reachable)", source)
		}
	}
	return cfg, source, nil
}

func defaultSetupConfig() setupConfig {
	return setupConfigFromFileConfig(configfile.Defaults())
}

func setupConfigFromFileConfig(cfg configfile.Config) setupConfig {
	interval := cfg.Interval
	if interval == "0s" {
		interval = "0"
	}
	cfg.Interval = interval
	return setupConfig{Config: cfg}
}

func setupConfigToFileConfig(cfg setupConfig) (configfile.Config, error) {
	return configfile.Normalize(cfg.Config)
}

func setupConfigFromEnv(env map[string]string) setupConfig {
	cfg := defaultSetupConfig()
	applySetupStringEnv(&cfg, env)
	applySetupBoolEnv(&cfg, env)
	applySetupIntEnv(&cfg, env)
	applySetupListenEnv(&cfg, env)
	return cfg
}

type setupStringEnv struct {
	key string
	set func(*setupConfig, string)
}

func applySetupStringEnv(cfg *setupConfig, env map[string]string) {
	overrides := []setupStringEnv{
		{"INDEXER_PATH", func(c *setupConfig, v string) { c.IndexPath = v }},
		{"INDEXER_NAME", func(c *setupConfig, v string) { c.IndexName = v }},
		{"INDEXER_DB_PATH", func(c *setupConfig, v string) { c.DBPath = v }},
		{"INDEXER_DB_BUSY_TIMEOUT", func(c *setupConfig, v string) { c.DBBusyTimeout = v }},
		{"INDEXER_DB_JOURNAL_MODE", func(c *setupConfig, v string) { c.DBJournalMode = v }},
		{"INDEXER_DB_SYNCHRONOUS", func(c *setupConfig, v string) { c.DBSynchronous = v }},
		{"INDEXER_DB_AUTO_VACUUM", func(c *setupConfig, v string) { c.DBAutoVacuum = v }},
		{"INDEXER_DB_CONN_MAX_IDLE_TIME", func(c *setupConfig, v string) { c.DBConnMaxIdleTime = v }},
		{"INDEXER_SOCKET", func(c *setupConfig, v string) { c.SocketPath = v }},
		{"INDEXER_INTERVAL", func(c *setupConfig, v string) { c.Interval = v }},
	}
	for _, override := range overrides {
		if value, ok := env[override.key]; ok {
			override.set(cfg, value)
		}
	}
}

type setupBoolEnv struct {
	key string
	set func(*setupConfig, bool)
}

func applySetupBoolEnv(cfg *setupConfig, env map[string]string) {
	overrides := []setupBoolEnv{
		{"INDEXER_INCLUDE_HIDDEN", func(c *setupConfig, v bool) { c.IncludeHidden = v }},
		{"INDEXER_INCLUDE_NETWORK_MOUNTS", func(c *setupConfig, v bool) { c.IncludeNetworkMounts = v }},
		{"INDEXER_FRESH", func(c *setupConfig, v bool) { c.FreshIndex = v }},
	}
	for _, override := range overrides {
		if value, ok := env[override.key]; ok {
			override.set(cfg, parseBoolWithFallback(value, currentSetupBool(cfg, override.key)))
		}
	}
}

func currentSetupBool(cfg *setupConfig, key string) bool {
	switch key {
	case "INDEXER_INCLUDE_HIDDEN":
		return cfg.IncludeHidden
	case "INDEXER_INCLUDE_NETWORK_MOUNTS":
		return cfg.IncludeNetworkMounts
	case "INDEXER_FRESH":
		return cfg.FreshIndex
	default:
		return false
	}
}

type setupIntEnv struct {
	key string
	set func(*setupConfig, int)
}

func applySetupIntEnv(cfg *setupConfig, env map[string]string) {
	overrides := []setupIntEnv{
		{"INDEXER_KEEP_INDEXES", func(c *setupConfig, v int) { c.KeepIndexes = v }},
		{"INDEXER_DB_MAX_OPEN_CONNS", func(c *setupConfig, v int) { c.DBMaxOpenConns = v }},
		{"INDEXER_DB_MAX_IDLE_CONNS", func(c *setupConfig, v int) { c.DBMaxIdleConns = v }},
	}
	for _, override := range overrides {
		if parsed, ok := parseSetupNonNegativeInt(env[override.key]); ok {
			override.set(cfg, parsed)
		}
	}
}

func parseSetupNonNegativeInt(raw string) (int, bool) {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	return parsed, err == nil && parsed >= 0
}

func applySetupListenEnv(cfg *setupConfig, env map[string]string) {
	if value, ok := env["INDEXER_LISTEN_FLAG"]; ok {
		cfg.ListenAddr, cfg.ListenFlag = splitListenFlag(value)
	}
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
		"INDEXER_DB_BUSY_TIMEOUT":        cfg.DBBusyTimeout,
		"INDEXER_DB_JOURNAL_MODE":        cfg.DBJournalMode,
		"INDEXER_DB_SYNCHRONOUS":         cfg.DBSynchronous,
		"INDEXER_DB_AUTO_VACUUM":         cfg.DBAutoVacuum,
		"INDEXER_DB_MAX_OPEN_CONNS":      strconv.Itoa(cfg.DBMaxOpenConns),
		"INDEXER_DB_MAX_IDLE_CONNS":      strconv.Itoa(cfg.DBMaxIdleConns),
		"INDEXER_DB_CONN_MAX_IDLE_TIME":  cfg.DBConnMaxIdleTime,
		"INDEXER_SOCKET":                 cfg.SocketPath,
		"INDEXER_INTERVAL":               cfg.Interval,
		"INDEXER_LISTEN_FLAG":            listenFlag,
	}
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
	for i := range setupFields {
		cfg, err = promptSetupField(reader, w, cfg, i+1)
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
		if err != nil || n < 1 || n > len(setupFields) {
			_, _ = fmt.Fprintf(w, "Enter a field number from 1 to %d, apply, or cancel.\n", len(setupFields))
			continue
		}
		cfg, err = promptSetupField(reader, w, cfg, n)
		if err != nil {
			return cfg, false, err
		}
	}
}

func printSetupSummary(w io.Writer, cfg setupConfig) error {
	var sb strings.Builder
	sb.WriteString("\nReview\n------\n")
	for i, f := range setupFields {
		fmt.Fprintf(&sb, " %2d. %s: %s\n", i+1, f.Label, f.Value(cfg))
	}
	if cfg.ListenFlag != "" && strings.TrimSpace(cfg.ListenAddr) == "" {
		fmt.Fprintf(&sb, "     Preserved INDEXER_LISTEN_FLAG: %s\n", cfg.ListenFlag)
	}
	sb.WriteByte('\n')
	_, err := io.WriteString(w, sb.String())
	return err
}

func promptSetupField(reader *bufio.Reader, w io.Writer, cfg setupConfig, field int) (setupConfig, error) {
	if field < 1 || field > len(setupFields) {
		return cfg, fmt.Errorf("unknown setup field %d", field)
	}
	return setupFields[field-1].Prompt(reader, w, cfg)
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
