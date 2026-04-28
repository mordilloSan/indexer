package cli

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

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
	envFile := fs.String("env-file", defaultEnvFile, "Environment file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	dryRun := fs.Bool("dry-run", false, "Print the resulting environment file without writing or restarting")
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
	if *dryRun {
		if err := printEnvDryRun(*envFile, env); err != nil {
			fmt.Fprintf(os.Stderr, "format %s: %v\n", *envFile, err)
			os.Exit(1)
		}
		return
	}

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
		source = "built-in defaults (daemon not reachable)"
	} else {
		source = fmt.Sprintf("%s (daemon not reachable)", source)
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
