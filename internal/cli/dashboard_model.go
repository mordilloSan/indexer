package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

type dashboardOptions struct {
	ConfigPath    string
	SocketPath    string
	SocketPathSet bool
	ListenAddr    string
	ListenAddrSet bool
	Refresh       time.Duration
	ServiceUnit   string
	SocketUnit    string
	TimerUnit     string
	TargetUnit    string
	IndexJobUnit  string
}

type dashboardSnapshot struct {
	Config       configfile.Config
	ConfigPath   string
	ConfigErr    string
	Status       dashboardAPIStatus
	StatusErr    string
	Units        []unitStatus
	CollectedAt  time.Time
	SelectedLogs string
	Logs         string
	LogErr       string
}

type dashboardAPIStatus struct {
	Status          string `json:"status"`
	NumDirs         int64  `json:"num_dirs"`
	NumFiles        int64  `json:"num_files"`
	TotalSize       int64  `json:"total_size"`
	LastIndexed     string `json:"last_indexed"`
	TotalIndexes    int    `json:"total_indexes"`
	TotalEntries    int64  `json:"total_entries"`
	DatabaseSize    int64  `json:"database_size"`
	WALSize         int64  `json:"wal_size"`
	SHMSize         int64  `json:"shm_size"`
	TotalOnDisk     int64  `json:"total_on_disk"`
	RSSBytes        int64  `json:"rss_bytes"`
	CgroupCurrent   int64  `json:"cgroup_memory_current_bytes"`
	CgroupAnon      int64  `json:"cgroup_memory_anon_bytes"`
	CgroupFile      int64  `json:"cgroup_memory_file_bytes"`
	ActiveOperation string `json:"active_operation,omitempty"`
	ActivePath      string `json:"active_path,omitempty"`
	Warning         string `json:"warning,omitempty"`
}

func defaultDashboardOptions() dashboardOptions {
	return dashboardOptions{
		ConfigPath:   configfile.PathFromEnvOrDefault(),
		SocketPath:   "/var/run/indexer.sock",
		Refresh:      2 * time.Second,
		ServiceUnit:  defaultServiceUnit,
		SocketUnit:   defaultSocketUnit,
		TimerUnit:    defaultTimerUnit,
		TargetUnit:   defaultTargetUnit,
		IndexJobUnit: defaultIndexServiceUnit,
	}
}

func collectDashboardSnapshot(opts dashboardOptions, selectedLogs string) dashboardSnapshot {
	snap := dashboardSnapshot{
		ConfigPath:   opts.ConfigPath,
		CollectedAt:  time.Now(),
		SelectedLogs: selectedLogs,
	}

	cfg, err := configfile.Load(opts.ConfigPath)
	configLoaded := false
	if err != nil {
		snap.ConfigErr = err.Error()
	} else {
		snap.Config = cfg
		configLoaded = true
	}

	socketPath, listenAddr := dashboardAPIConnection(opts, cfg, configLoaded)
	body, err := fetchDaemonBody(socketPath, listenAddr, "/status")
	if err != nil {
		snap.StatusErr = err.Error()
	} else if err := json.Unmarshal(body, &snap.Status); err != nil {
		snap.StatusErr = fmt.Sprintf("parse /status: %v", err)
	}

	units := []string{opts.TargetUnit, opts.SocketUnit, opts.ServiceUnit, opts.TimerUnit, opts.IndexJobUnit}
	snap.Units = collectUnitStatuses(units)

	if selectedLogs != "" {
		logs, err := journalctlUnitLogs(selectedLogs, 80)
		if err != nil {
			snap.LogErr = err.Error()
		}
		snap.Logs = logs
	}

	return snap
}

func dashboardAPIConnection(opts dashboardOptions, cfg configfile.Config, configLoaded bool) (string, string) {
	socketPath := opts.SocketPath
	listenAddr := opts.ListenAddr
	if configLoaded {
		if !opts.SocketPathSet {
			socketPath = cfg.SocketPath
			if strings.TrimSpace(socketPath) == "" {
				socketPath = "-"
			}
		}
		if !opts.ListenAddrSet {
			listenAddr = cfg.ListenAddr
		}
	}
	return socketPath, listenAddr
}

func dashboardSummary(snap dashboardSnapshot) string {
	api := "unreachable"
	if snap.StatusErr == "" && snap.Status.Status != "" {
		api = snap.Status.Status
		if snap.Status.ActiveOperation != "" {
			api += " (" + snap.Status.ActiveOperation + ")"
		}
	}
	timer := unitActiveState(snap.Units, defaultTimerUnit)
	socket := unitActiveState(snap.Units, defaultSocketUnit)
	return fmt.Sprintf("Indexer  API: %s  Socket: %s  Timer: %s  Updated: %s",
		api, socket, timer, snap.CollectedAt.Format("15:04:05"))
}

func dashboardDetails(snap dashboardSnapshot) string {
	var b strings.Builder
	b.WriteString("[::b]Configuration[::-]\n")
	if snap.ConfigErr != "" {
		appendf(&b, "Config: %s\nError: %s\n\n", snap.ConfigPath, snap.ConfigErr)
	} else {
		appendf(&b, "Config: %s\nPath:   %s\nDB:     %s\nSocket: %s\nTCP:    %s\nTimer:  %s\n\n",
			snap.ConfigPath,
			snap.Config.IndexPath,
			snap.Config.DBPath,
			disabledLabel(snap.Config.SocketPath),
			disabledLabel(snap.Config.ListenAddr),
			snap.Config.Interval)
	}

	b.WriteString("[::b]API status[::-]\n")
	if snap.StatusErr != "" {
		appendf(&b, "Error: %s\n\n", snap.StatusErr)
	} else {
		appendf(&b, "Status: %s\nFiles:  %d\nDirs:   %d\nEntries:%d\nDB:     %s on disk\nLast:   %s\n",
			snap.Status.Status,
			snap.Status.NumFiles,
			snap.Status.NumDirs,
			snap.Status.TotalEntries,
			formatBytes(snap.Status.TotalOnDisk),
			emptyLabel(snap.Status.LastIndexed))
		if snap.Status.ActiveOperation != "" {
			appendf(&b, "Active: %s %s\n", snap.Status.ActiveOperation, snap.Status.ActivePath)
		}
		if snap.Status.Warning != "" {
			appendf(&b, "Warning: %s\n", snap.Status.Warning)
		}
		b.WriteByte('\n')
	}

	b.WriteString("[::b]Systemd units[::-]\n")
	for _, unit := range snap.Units {
		appendf(&b, "%-28s %-9s %-11s %-12s %s\n", unit.Name, unit.LoadState, unit.ActiveState, unit.SubState, unit.Result)
	}

	if snap.SelectedLogs != "" {
		appendf(&b, "\n[::b]Logs: %s[::-]\n", snap.SelectedLogs)
		if snap.LogErr != "" {
			appendf(&b, "Error: %s\n", snap.LogErr)
		}
		if strings.TrimSpace(snap.Logs) == "" {
			b.WriteString("No log lines.\n")
		} else {
			b.WriteString(snap.Logs)
		}
	}

	return b.String()
}

func unitActiveState(units []unitStatus, name string) string {
	for _, unit := range units {
		if unit.Name == name {
			return unit.ActiveState
		}
	}
	return "unknown"
}

func disabledLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "disabled"
	}
	return value
}

func emptyLabel(value string) string {
	if strings.TrimSpace(value) == "" {
		return "never"
	}
	return value
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
