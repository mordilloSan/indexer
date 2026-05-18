package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

type statusOverviewOptions struct {
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

type statusOverviewSnapshot struct {
	Config      configfile.Config
	ConfigPath  string
	ConfigErr   string
	Status      statusOverviewAPIStatus
	StatusErr   string
	Units       []unitStatus
	CollectedAt time.Time
}

type statusOverviewAPIStatus struct {
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

func defaultStatusOverviewOptions() statusOverviewOptions {
	return statusOverviewOptions{
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

func collectStatusOverviewSnapshot(opts statusOverviewOptions) statusOverviewSnapshot {
	snap := statusOverviewSnapshot{
		ConfigPath:  opts.ConfigPath,
		CollectedAt: time.Now(),
	}

	cfg, err := configfile.Load(opts.ConfigPath)
	configLoaded := false
	if err != nil {
		snap.ConfigErr = err.Error()
	} else {
		snap.Config = cfg
		configLoaded = true
	}

	socketPath, listenAddr := statusOverviewAPIConnection(opts, cfg, configLoaded)
	body, err := fetchDaemonBody(socketPath, listenAddr, "/status")
	if err != nil {
		snap.StatusErr = err.Error()
	} else if err := json.Unmarshal(body, &snap.Status); err != nil {
		snap.StatusErr = fmt.Sprintf("parse /status: %v", err)
	}

	units := []string{opts.TargetUnit, opts.SocketUnit, opts.ServiceUnit, opts.TimerUnit, opts.IndexJobUnit}
	snap.Units = collectUnitStatuses(units)

	return snap
}

func statusOverviewAPIConnection(opts statusOverviewOptions, cfg configfile.Config, configLoaded bool) (string, string) {
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

func statusOverviewHeader(snap statusOverviewSnapshot) string {
	api := "unreachable"
	if snap.StatusErr == "" && snap.Status.Status != "" {
		api = snap.Status.Status
		if snap.Status.ActiveOperation != "" {
			api += " (" + snap.Status.ActiveOperation + ")"
		}
	}
	timer := unitActiveState(snap.Units, defaultTimerUnit)
	socket := unitActiveState(snap.Units, defaultSocketUnit)
	return fmt.Sprintf("API: %s  Socket: %s  Timer: %s", api, socket, timer)
}

func statusOverviewText(snap statusOverviewSnapshot, width int) string {
	var b strings.Builder
	b.WriteString("Indexer\n")
	b.WriteString(statusOverviewHeader(snap))
	b.WriteString("\n\n")
	b.WriteString(statusOverviewDetails(snap, width))
	return b.String()
}

func statusOverviewDetails(snap statusOverviewSnapshot, width int) string {
	width = normalizedStatusOverviewWidth(width)
	var b strings.Builder
	appendLines(&b, statusOverviewConfigLines(snap, width))
	b.WriteByte('\n')
	appendLines(&b, statusOverviewAPILines(snap, width))
	return b.String()
}

type statusOverviewKV struct {
	Label string
	Value string
}

func statusOverviewItems(snap statusOverviewSnapshot) []statusOverviewKV {
	return []statusOverviewKV{
		{"Status", snap.Status.Status},
		{"Files", fmt.Sprintf("%d", snap.Status.NumFiles)},
		{"Dirs", fmt.Sprintf("%d", snap.Status.NumDirs)},
		{"Indexed", formatBytes(snap.Status.TotalSize)},
		{"Last", emptyLabel(snap.Status.LastIndexed)},
		{"Indexes", fmt.Sprintf("%d", snap.Status.TotalIndexes)},
		{"DB main", formatBytes(snap.Status.DatabaseSize)},
		{"WAL", formatBytes(snap.Status.WALSize)},
	}
}

func statusOverviewConfigLines(snap statusOverviewSnapshot, width int) []string {
	lines := []string{"Configuration"}
	if snap.ConfigErr != "" {
		return append(lines, statusOverviewKeyValueLines(width, []statusOverviewKV{
			{"Config", snap.ConfigPath},
			{"Error", snap.ConfigErr},
		})...)
	}

	return append(lines, statusOverviewKeyValueLines(width, []statusOverviewKV{
		{"Config", snap.ConfigPath},
		{"Path", snap.Config.IndexPath},
		{"DB", snap.Config.DBPath},
		{"Socket", disabledLabel(snap.Config.SocketPath)},
		{"TCP", disabledLabel(snap.Config.ListenAddr)},
		{"Timer", snap.Config.Interval},
	})...)
}

func statusOverviewAPILines(snap statusOverviewSnapshot, width int) []string {
	lines := []string{"API status"}
	if snap.StatusErr != "" {
		return append(lines, statusOverviewKeyValueLines(width, []statusOverviewKV{{"Error", snap.StatusErr}})...)
	}
	items := statusOverviewItems(snap)
	if snap.Status.ActiveOperation != "" {
		items = append(items, statusOverviewKV{"Active", snap.Status.ActiveOperation + " " + snap.Status.ActivePath})
	}
	if snap.Status.Warning != "" {
		items = append(items, statusOverviewKV{"Warning", snap.Status.Warning})
	}
	return append(lines, statusOverviewKeyValueLines(width, items)...)
}

func statusOverviewKeyValueLines(width int, items []statusOverviewKV) []string {
	const labelWidth = 10
	lines := make([]string, 0, len(items))
	valueWidth := max(width-labelWidth, 0)
	for _, item := range items {
		label := fmt.Sprintf("%-*s", labelWidth, item.Label+":")
		if valueWidth == 0 {
			lines = append(lines, truncateCell(strings.TrimSpace(label), width))
			continue
		}
		lines = append(lines, label+truncateCell(item.Value, valueWidth))
	}
	return lines
}

func truncateCell(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 1 {
		return value[:limit]
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func appendLines(b *strings.Builder, lines []string) {
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func normalizedStatusOverviewWidth(width int) int {
	if width <= 0 {
		return 80
	}
	return width
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
