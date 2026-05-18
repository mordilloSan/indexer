package cli

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

const (
	defaultSocketUnit = "indexer.socket"
	defaultTimerUnit  = "indexer-index.timer"
	defaultTargetUnit = "indexer.target"
)

var (
	systemdSystemDir    = "/etc/systemd/system"
	systemCommandOutput = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

func runConfigApply(args []string) {
	fs := flag.NewFlagSet("config apply", flag.ExitOnError)
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file to apply")
	service := fs.String("service", defaultServiceUnit, "Systemd service unit name")
	socketUnit := fs.String("socket-unit", defaultSocketUnit, "Systemd socket unit name")
	timer := fs.String("timer", defaultTimerUnit, "Systemd index timer unit name")
	target := fs.String("target", defaultTargetUnit, "Systemd target unit name")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	cfg, err := configfile.Load(*configPath)
	if err != nil {
		writefOrExit(os.Stderr, "read %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	if err := applySystemdConfig(cfg, *service, *socketUnit, *timer, *target); err != nil {
		writelnOrExit(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func runConfigSet(args []string) {
	fs := flag.NewFlagSet("config set", flag.ExitOnError)
	configPath := fs.String("config-file", configfile.PathFromEnvOrDefault(), "JSON config file to update")
	noRestart := fs.Bool("no-restart", false, "Skip service restart after updating")
	dryRun := fs.Bool("dry-run", false, "Print the resulting config file without writing or restarting")
	service := fs.String("service", defaultServiceUnit, "Systemd service unit name")
	socketUnit := fs.String("socket-unit", defaultSocketUnit, "Systemd socket unit name")
	timer := fs.String("timer", defaultTimerUnit, "Systemd index timer unit name")
	target := fs.String("target", defaultTargetUnit, "Systemd target unit name")

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
		writelnOrExit(os.Stderr, "no settings specified; use flags like --interval=2h or --path=/data")
		fs.Usage()
		os.Exit(1)
	}

	current, err := configfile.Load(*configPath)
	if err != nil {
		writefOrExit(os.Stderr, "read %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	next, err := configfile.ApplyPatch(current, patch)
	if err != nil {
		writelnOrExit(os.Stderr, err.Error())
		os.Exit(1)
	}

	if *dryRun {
		if err := printConfigDryRun(*configPath, next); err != nil {
			writefOrExit(os.Stderr, "format %s: %v\n", *configPath, err)
			os.Exit(1)
		}
		return
	}

	if err := configfile.Save(*configPath, next); err != nil {
		writefOrExit(os.Stderr, "write %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	writefOrExit(os.Stdout, "updated %s\n", *configPath)

	if !*noRestart {
		if err := applySystemdConfigChanges(patch, next, *service, *socketUnit, *timer, *target); err != nil {
			writelnOrExit(os.Stderr, err.Error())
			os.Exit(1)
		}
	}
}

func printConfigDryRun(configPath string, cfg configfile.Config) error {
	content, err := configfile.Format(cfg)
	if err != nil {
		return err
	}
	writefOrExit(os.Stdout, "dry run: would write %s:\n%s", configPath, content)
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

func applySystemdConfigChanges(patch configfile.Patch, cfg configfile.Config, service, socketUnit, timer, target string) error {
	targetNeedsUpdate := false
	if patch.Interval != nil {
		if err := applySystemdTimerInterval(cfg.Interval, timer); err != nil {
			return err
		}
		targetNeedsUpdate = true
	}
	if patch.SocketPath != nil {
		if err := applySystemdSocketPath(cfg.SocketPath, socketUnit); err != nil {
			return err
		}
		targetNeedsUpdate = true
	}
	if targetNeedsUpdate {
		if err := applySystemdTargetWants(cfg, target, socketUnit, timer); err != nil {
			return err
		}
	}
	if err := runSystemctl("try-restart", service); err != nil {
		return err
	}
	return nil
}

func applySystemdConfig(cfg configfile.Config, service, socketUnit, timer, target string) error {
	patch := configfile.Patch{
		SocketPath: &cfg.SocketPath,
		Interval:   &cfg.Interval,
	}
	return applySystemdConfigChanges(patch, cfg, service, socketUnit, timer, target)
}

func applySystemdSocketPath(socketPath, socketUnit string) error {
	if strings.TrimSpace(socketPath) == "" {
		if err := runSystemctl("disable", "--now", socketUnit); err != nil {
			return err
		}
		writefOrExit(os.Stdout, "disabled %s\n", socketUnit)
		return nil
	}
	if err := writeSocketOverride(socketUnit, socketPath); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", socketUnit); err != nil {
		return err
	}
	if err := runSystemctl("restart", socketUnit); err != nil {
		return err
	}
	writefOrExit(os.Stdout, "updated %s path to %s\n", socketUnit, socketPath)
	return nil
}

func applySystemdTimerInterval(interval, timer string) error {
	duration, err := configfile.ParseInterval(interval)
	if err != nil {
		return err
	}
	if duration <= 0 {
		if err := runSystemctl("disable", "--now", timer); err != nil {
			return err
		}
		writefOrExit(os.Stdout, "disabled %s\n", timer)
		return nil
	}

	if err := writeTimerOverride(timer, duration); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctl("enable", "--now", timer); err != nil {
		return err
	}
	if err := runSystemctl("restart", timer); err != nil {
		return err
	}
	writefOrExit(os.Stdout, "updated %s interval to %s\n", timer, duration)
	return nil
}

func applySystemdTargetWants(cfg configfile.Config, target, socketUnit, timer string) error {
	wants, err := targetWantsForConfig(cfg, socketUnit, timer)
	if err != nil {
		return err
	}
	if err := writeTargetOverride(target, wants); err != nil {
		return err
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return err
	}
	wantsLabel := strings.Join(wants, " ")
	if wantsLabel == "" {
		wantsLabel = "(none)"
	}
	writefOrExit(os.Stdout, "updated %s wants to %s\n", target, wantsLabel)
	return nil
}

func targetWantsForConfig(cfg configfile.Config, socketUnit, timer string) ([]string, error) {
	var wants []string
	if strings.TrimSpace(cfg.SocketPath) != "" {
		wants = append(wants, socketUnit)
	}
	duration, err := configfile.ParseInterval(cfg.Interval)
	if err != nil {
		return nil, err
	}
	if duration > 0 {
		wants = append(wants, timer)
	}
	return wants, nil
}

func writeTimerOverride(timer string, interval time.Duration) error {
	dir := filepath.Join(systemdSystemDir, timer+".d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create timer override directory: %w", err)
	}
	content := "[Timer]\nOnUnitActiveSec=\nOnUnitActiveSec=" + systemdDuration(interval) + "\n"
	path := filepath.Join(dir, "override.conf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write timer override %s: %w", path, err)
	}
	return nil
}

func writeSocketOverride(socketUnit, socketPath string) error {
	dir := filepath.Join(systemdSystemDir, socketUnit+".d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create socket override directory: %w", err)
	}
	content := "[Socket]\nListenStream=\nListenStream=" + socketPath + "\n"
	path := filepath.Join(dir, "override.conf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write socket override %s: %w", path, err)
	}
	return nil
}

func writeTargetOverride(target string, wants []string) error {
	dir := filepath.Join(systemdSystemDir, target+".d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create target override directory: %w", err)
	}
	content := "[Unit]\nWants=\n"
	if len(wants) > 0 {
		content += "Wants=" + strings.Join(wants, " ") + "\n"
	}
	path := filepath.Join(dir, "override.conf")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write target override %s: %w", path, err)
	}
	return nil
}

func systemdDuration(duration time.Duration) string {
	if duration <= 0 {
		return "0"
	}
	seconds := duration / time.Second
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10) + "s"
}

func runSystemctl(args ...string) error {
	out, err := systemCommandOutput("systemctl", args...)
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
