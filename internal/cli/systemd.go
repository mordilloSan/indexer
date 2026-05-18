package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const serviceUsageText = `Usage: indexer service <command> [flags]

Commands:
  status   Show systemd state for indexer target/socket/services/timer
  logs     Print journal logs for a unit or alias
  run-now  Start the one-shot indexing service

Examples:
  indexer service status
  indexer service logs api
  indexer service logs index --lines 120
  indexer service run-now
`

var managedUnits = []string{
	defaultTargetUnit,
	defaultSocketUnit,
	defaultServiceUnit,
	defaultTimerUnit,
	defaultIndexServiceUnit,
}

type unitStatus struct {
	Name          string
	LoadState     string
	ActiveState   string
	SubState      string
	UnitFileState string
	Result        string
	NextElapse    string
}

func runService(args []string) {
	if len(args) < 1 {
		writeOrExit(os.Stderr, serviceUsageText)
		os.Exit(1)
	}

	switch args[0] {
	case "--help", "-h", "help":
		writeOrExit(os.Stdout, serviceUsageText)
	case "status":
		runServiceStatus(args[1:])
	case "logs":
		runServiceLogs(args[1:])
	case "run-now":
		runServiceRunNow(args[1:])
	default:
		writefOrExit(os.Stderr, "unknown service command: %s\n", args[0])
		os.Exit(1)
	}
}

func runServiceStatus(args []string) {
	fs := flag.NewFlagSet("service status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	statuses := collectUnitStatuses(managedUnits)
	printUnitStatuses(statuses)
}

func runServiceLogs(args []string) {
	fs := flag.NewFlagSet("service logs", flag.ExitOnError)
	unit := fs.String("unit", defaultServiceUnit, "Systemd unit to read")
	lines := fs.Int("lines", 80, "Number of journal lines")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if fs.NArg() > 0 {
		*unit = serviceLogUnit(fs.Arg(0))
	}

	out, err := journalctlUnitLogs(*unit, *lines)
	if err != nil {
		writelnOrExit(os.Stderr, err.Error())
		os.Exit(1)
	}
	writeOrExit(os.Stdout, out)
}

func runServiceRunNow(args []string) {
	fs := flag.NewFlagSet("service run-now", flag.ExitOnError)
	unit := fs.String("unit", defaultIndexServiceUnit, "Systemd indexing service unit")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if err := runSystemctl("start", *unit); err != nil {
		writelnOrExit(os.Stderr, err.Error())
		os.Exit(1)
	}
	writefOrExit(os.Stdout, "started %s\n", *unit)
}

func serviceLogUnit(alias string) string {
	switch strings.ToLower(strings.TrimSpace(alias)) {
	case "api", "daemon", "service":
		return defaultServiceUnit
	case "index", "indexer", "job":
		return defaultIndexServiceUnit
	case "timer":
		return defaultTimerUnit
	case "socket":
		return defaultSocketUnit
	case "target":
		return defaultTargetUnit
	default:
		return alias
	}
}

func collectUnitStatuses(units []string) []unitStatus {
	statuses := make([]unitStatus, 0, len(units))
	for _, unit := range units {
		status, err := systemctlUnitStatus(unit)
		if err != nil {
			status = unitStatus{Name: unit, LoadState: "error", ActiveState: "unknown", Result: err.Error()}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func systemctlUnitStatus(unit string) (unitStatus, error) {
	args := []string{
		"show",
		"--property=Id",
		"--property=LoadState",
		"--property=ActiveState",
		"--property=SubState",
		"--property=UnitFileState",
		"--property=Result",
		"--property=NextElapseUSecRealtime",
		unit,
	}
	out, err := systemCommandOutput("systemctl", args...)
	status := parseSystemctlShow(unit, string(out))
	if err != nil {
		return status, fmt.Errorf("systemctl show %s failed: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return status, nil
}

func parseSystemctlShow(fallbackName, raw string) unitStatus {
	values := map[string]string{}
	for line := range strings.SplitSeq(raw, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[key] = value
	}
	status := unitStatus{
		Name:          fallbackName,
		LoadState:     valueOrUnknown(values["LoadState"]),
		ActiveState:   valueOrUnknown(values["ActiveState"]),
		SubState:      valueOrUnknown(values["SubState"]),
		UnitFileState: valueOrUnknown(values["UnitFileState"]),
		Result:        valueOrUnknown(values["Result"]),
		NextElapse:    values["NextElapseUSecRealtime"],
	}
	if id := strings.TrimSpace(values["Id"]); id != "" {
		status.Name = id
	}
	return status
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func printUnitStatuses(statuses []unitStatus) {
	sort.SliceStable(statuses, func(i, j int) bool {
		return unitOrder(statuses[i].Name) < unitOrder(statuses[j].Name)
	})
	for _, status := range statuses {
		writefOrExit(os.Stdout, "%-28s %-10s %-12s %-14s %s\n", status.Name, status.LoadState, status.ActiveState, status.SubState, status.Result)
	}
}

func unitOrder(unit string) int {
	for i, known := range managedUnits {
		if unit == known {
			return i
		}
	}
	return len(managedUnits)
}

func journalctlUnitLogs(unit string, lines int) (string, error) {
	if lines <= 0 {
		lines = 80
	}
	out, err := systemCommandOutput("journalctl", "-u", unit, "-n", strconv.Itoa(lines), "--no-pager")
	if err != nil {
		return string(out), fmt.Errorf("journalctl %s failed: %w: %s", unit, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
