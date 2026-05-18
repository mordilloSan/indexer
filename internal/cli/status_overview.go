package cli

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mordilloSan/indexer/internal/configfile"
)

func runStatusOverview(command string, args []string) {
	defaults := defaultStatusOverviewOptions()
	fs := flag.NewFlagSet(command, flag.ExitOnError)
	configPath := fs.String("config-file", defaults.ConfigPath, "JSON config file path")
	socketPath := fs.String("socket-path", defaults.SocketPath, "Unix socket path")
	listenAddr := fs.String("listen", "", "TCP address of the daemon (e.g., :8080)")
	refreshRaw := fs.String("refresh", defaults.Refresh.String(), "Refresh interval for --watch")
	watch := fs.Bool("watch", false, "Refresh until interrupted")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	flagSeen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		flagSeen[f.Name] = true
	})

	refresh, err := configfile.ParseInterval(*refreshRaw)
	if err != nil || refresh <= 0 {
		writefOrExit(os.Stderr, "invalid --refresh %q\n", *refreshRaw)
		os.Exit(1)
	}

	opts := defaults
	opts.ConfigPath = *configPath
	opts.SocketPath = *socketPath
	opts.SocketPathSet = flagSeen["socket-path"]
	opts.ListenAddr = *listenAddr
	opts.ListenAddrSet = flagSeen["listen"]
	opts.Refresh = refresh

	if *watch {
		runStatusOverviewWatch(opts)
		return
	}
	printStatusOverview(opts)
}

func runStatusOverviewWatch(opts statusOverviewOptions) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for {
		writeOrExit(os.Stdout, "\x1b[H\x1b[2J")
		printStatusOverview(opts)

		timer := time.NewTimer(opts.Refresh)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func printStatusOverview(opts statusOverviewOptions) {
	snap := collectStatusOverviewSnapshot(opts)
	writeOrExit(os.Stdout, statusOverviewText(snap, statusOverviewOutputWidth()))
}

func statusOverviewOutputWidth() int {
	const fallbackWidth = 100
	raw := strings.TrimSpace(os.Getenv("COLUMNS"))
	if raw == "" {
		return fallbackWidth
	}
	width, err := strconv.Atoi(raw)
	if err != nil || width < 40 {
		return fallbackWidth
	}
	return width
}
