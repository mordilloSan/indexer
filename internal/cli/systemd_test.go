package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

func TestParseSystemctlShow(t *testing.T) {
	status := parseSystemctlShow("fallback.service", strings.Join([]string{
		"Id=indexer.service",
		"LoadState=loaded",
		"ActiveState=inactive",
		"SubState=dead",
		"UnitFileState=static",
		"Result=success",
		"NextElapseUSecRealtime=Mon 2026-05-18 09:00:00 BST",
	}, "\n"))

	if status.Name != "indexer.service" ||
		status.LoadState != "loaded" ||
		status.ActiveState != "inactive" ||
		status.SubState != "dead" ||
		status.UnitFileState != "static" ||
		status.Result != "success" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestApplySystemdConfigWritesOverridesAndCommands(t *testing.T) {
	dir := t.TempDir()
	restoreDir := systemdSystemDir
	restoreCommand := systemCommandOutput
	systemdSystemDir = dir
	defer func() {
		systemdSystemDir = restoreDir
		systemCommandOutput = restoreCommand
	}()

	var calls [][]string
	systemCommandOutput = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}

	cfg := configfile.Defaults()
	cfg.SocketPath = "/run/custom/indexer.sock"
	cfg.Interval = "30m0s"
	if err := applySystemdConfig(cfg, defaultServiceUnit, defaultSocketUnit, defaultTimerUnit, defaultTargetUnit); err != nil {
		t.Fatalf("applySystemdConfig: %v", err)
	}

	timerOverride := readTestFile(t, filepath.Join(dir, defaultTimerUnit+".d", "override.conf"))
	if !strings.Contains(timerOverride, "OnUnitActiveSec=1800s") {
		t.Fatalf("timer override = %q", timerOverride)
	}
	socketOverride := readTestFile(t, filepath.Join(dir, defaultSocketUnit+".d", "override.conf"))
	if !strings.Contains(socketOverride, "ListenStream=/run/custom/indexer.sock") {
		t.Fatalf("socket override = %q", socketOverride)
	}
	targetOverride := readTestFile(t, filepath.Join(dir, defaultTargetUnit+".d", "override.conf"))
	if !strings.Contains(targetOverride, "Wants="+defaultSocketUnit+" "+defaultTimerUnit) {
		t.Fatalf("target override = %q", targetOverride)
	}

	wantSuffix := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", defaultTimerUnit},
		{"systemctl", "restart", defaultTimerUnit},
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", defaultSocketUnit},
		{"systemctl", "restart", defaultSocketUnit},
		{"systemctl", "daemon-reload"},
		{"systemctl", "try-restart", defaultServiceUnit},
	}
	if !reflect.DeepEqual(calls, wantSuffix) {
		t.Fatalf("systemctl calls = %#v, want %#v", calls, wantSuffix)
	}
}

func TestApplySystemdConfigCanRemoveTargetWants(t *testing.T) {
	dir := t.TempDir()
	restoreDir := systemdSystemDir
	restoreCommand := systemCommandOutput
	systemdSystemDir = dir
	defer func() {
		systemdSystemDir = restoreDir
		systemCommandOutput = restoreCommand
	}()

	systemCommandOutput = func(name string, args ...string) ([]byte, error) {
		return nil, nil
	}

	cfg := configfile.Defaults()
	cfg.SocketPath = ""
	cfg.Interval = "0"
	if err := applySystemdConfig(cfg, defaultServiceUnit, defaultSocketUnit, defaultTimerUnit, defaultTargetUnit); err != nil {
		t.Fatalf("applySystemdConfig: %v", err)
	}

	targetOverride := readTestFile(t, filepath.Join(dir, defaultTargetUnit+".d", "override.conf"))
	if strings.TrimSpace(targetOverride) != "[Unit]\nWants=" {
		t.Fatalf("target override = %q", targetOverride)
	}
}

func TestServiceRunNowStartsIndexJob(t *testing.T) {
	restoreCommand := systemCommandOutput
	defer func() { systemCommandOutput = restoreCommand }()

	var calls [][]string
	systemCommandOutput = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}

	runServiceRunNow(nil)

	want := [][]string{{"systemctl", "start", defaultIndexServiceUnit}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("systemctl calls = %#v, want %#v", calls, want)
	}
}

func TestCollectDashboardSnapshotUsesConfigAPIAndSystemd(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "indexer.json")
	cfg := configfile.Defaults()
	cfg.IndexPath = "/srv/data"
	cfg.DBPath = filepath.Join(dir, "index.db")
	cfg.SocketPath = filepath.Join(dir, "indexer.sock")
	if err := configfile.Save(configPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	restoreCommand := systemCommandOutput
	restoreFetcher := daemonBodyFetcher
	defer func() {
		systemCommandOutput = restoreCommand
		daemonBodyFetcher = restoreFetcher
	}()

	systemCommandOutput = func(name string, args ...string) ([]byte, error) {
		if name == "journalctl" {
			return []byte("api log line\n"), nil
		}
		unit := args[len(args)-1]
		return []byte("Id=" + unit + "\nLoadState=loaded\nActiveState=active\nSubState=running\nUnitFileState=enabled\nResult=success\n"), nil
	}
	var seenSocketPath string
	daemonBodyFetcher = func(method, socketPath, listenAddr, endpoint string) ([]byte, error) {
		seenSocketPath = socketPath
		return []byte(`{"status":"idle","num_files":12,"num_dirs":3,"total_entries":15,"total_on_disk":2048}`), nil
	}

	opts := defaultDashboardOptions()
	opts.ConfigPath = configPath
	snap := collectDashboardSnapshot(opts, defaultServiceUnit)

	if snap.Config.IndexPath != "/srv/data" {
		t.Fatalf("IndexPath = %q, want /srv/data", snap.Config.IndexPath)
	}
	if snap.Status.Status != "idle" || snap.Status.NumFiles != 12 {
		t.Fatalf("unexpected API status: %#v", snap.Status)
	}
	if seenSocketPath != cfg.SocketPath {
		t.Fatalf("socketPath = %q, want %q", seenSocketPath, cfg.SocketPath)
	}
	if len(snap.Units) != 5 {
		t.Fatalf("len(Units) = %d, want 5", len(snap.Units))
	}
	if !strings.Contains(snap.Logs, "api log line") {
		t.Fatalf("Logs = %q", snap.Logs)
	}
	if time.Since(snap.CollectedAt) > time.Minute {
		t.Fatalf("CollectedAt looks stale: %s", snap.CollectedAt)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
