package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func runConfigSet(args []string) {
	fs := flag.NewFlagSet("config set", flag.ExitOnError)
	envFile := fs.String("env-file", defaultEnvFile, "Environment file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	dryRun := fs.Bool("dry-run", false, "Print the resulting environment file without writing or restarting")
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

	if *dryRun {
		if err := printEnvDryRun(*envFile, current); err != nil {
			fmt.Fprintf(os.Stderr, "format %s: %v\n", *envFile, err)
			os.Exit(1)
		}
		return
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

func printEnvDryRun(envFile string, env map[string]string) error {
	content, err := formatEnvFile(env)
	if err != nil {
		return err
	}
	fmt.Printf("dry run: would write %s:\n%s", envFile, content)
	return nil
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
