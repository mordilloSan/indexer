package cli

import (
	"flag"
	"os"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/mordilloSan/indexer/internal/configfile"
	"github.com/rivo/tview"
)

func runDashboard(args []string) {
	defaults := defaultDashboardOptions()
	fs := flag.NewFlagSet("dashboard", flag.ExitOnError)
	configPath := fs.String("config-file", defaults.ConfigPath, "JSON config file path")
	socketPath := fs.String("socket-path", defaults.SocketPath, "Unix socket path")
	listenAddr := fs.String("listen", "", "TCP address of the daemon (e.g., :8080)")
	refreshRaw := fs.String("refresh", defaults.Refresh.String(), "Refresh interval")
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

	if err := newDashboard(opts).run(); err != nil {
		writelnOrExit(os.Stderr, err.Error())
		os.Exit(1)
	}
}

type dashboard struct {
	opts          dashboardOptions
	app           *tview.Application
	header        *tview.TextView
	detail        *tview.TextView
	footer        *tview.TextView
	actions       *tview.List
	selectedLogs  string
	refreshing    atomic.Bool
	statusMessage string
}

func newDashboard(opts dashboardOptions) *dashboard {
	app := tview.NewApplication()
	d := &dashboard{
		opts:    opts,
		app:     app,
		header:  tview.NewTextView().SetDynamicColors(true),
		detail:  tview.NewTextView().SetDynamicColors(true).SetScrollable(true).SetWrap(false),
		footer:  tview.NewTextView().SetDynamicColors(true),
		actions: tview.NewList().ShowSecondaryText(false),
	}
	d.configure()
	return d
}

func (d *dashboard) configure() {
	d.header.SetBorder(true).SetTitle(" Indexer ")
	d.detail.SetBorder(true).SetTitle(" Details ")
	d.footer.SetText(" r refresh   q quit   arrows/j/k move   enter select ")
	d.actions.SetBorder(true).SetTitle(" Actions ")

	d.addAction("Overview", func() {
		d.selectedLogs = ""
		d.refreshAsync()
	})
	d.addConfirmAction("Run index now", "Start indexer-index.service now?", func() error {
		return runSystemctl("start", d.opts.IndexJobUnit)
	})
	d.addConfirmAction("Apply config", "Apply config to systemd socket/timer overrides?", func() error {
		cfg, err := configfile.Load(d.opts.ConfigPath)
		if err != nil {
			return err
		}
		return applySystemdConfig(cfg, d.opts.ServiceUnit, d.opts.SocketUnit, d.opts.TimerUnit, d.opts.TargetUnit)
	})
	d.addConfirmAction("Stop API daemon", "Stop the disposable API daemon?", func() error {
		return runSystemctl("stop", d.opts.ServiceUnit)
	})
	d.addConfirmAction("Restart target", "Restart indexer.target?", func() error {
		return runSystemctl("restart", d.opts.TargetUnit)
	})
	d.addConfirmAction("Vacuum database", "Start /vacuum through the API?", func() error {
		socketPath, listenAddr, err := d.apiConnection()
		if err != nil {
			return err
		}
		_, err = postDaemonBody(socketPath, listenAddr, "/vacuum")
		return err
	})
	d.addConfirmAction("Prune old indexes", "Start /prune through the API?", func() error {
		socketPath, listenAddr, err := d.apiConnection()
		if err != nil {
			return err
		}
		_, err = postDaemonBody(socketPath, listenAddr, "/prune")
		return err
	})
	d.addAction("API logs", func() {
		d.selectedLogs = d.opts.ServiceUnit
		d.refreshAsync()
	})
	d.addAction("Index logs", func() {
		d.selectedLogs = d.opts.IndexJobUnit
		d.refreshAsync()
	})

	d.actions.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'j':
			d.moveAction(1)
			return nil
		case 'k':
			d.moveAction(-1)
			return nil
		}
		return event
	})

	d.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyCtrlC:
			d.app.Stop()
			return nil
		}
		switch event.Rune() {
		case 'q':
			d.app.Stop()
			return nil
		case 'r':
			d.refreshAsync()
			return nil
		}
		return event
	})
}

func (d *dashboard) apiConnection() (string, string, error) {
	cfg, err := configfile.Load(d.opts.ConfigPath)
	if err != nil {
		return "", "", err
	}
	socketPath, listenAddr := dashboardAPIConnection(d.opts, cfg, true)
	return socketPath, listenAddr, nil
}

func (d *dashboard) addAction(label string, run func()) {
	d.actions.AddItem(label, "", 0, run)
}

func (d *dashboard) addConfirmAction(label, prompt string, run func() error) {
	d.actions.AddItem(label, "", 0, func() {
		d.confirm(prompt, func() {
			if err := run(); err != nil {
				d.statusMessage = err.Error()
			} else {
				d.statusMessage = label + " complete"
			}
			d.refreshAsync()
		})
	})
}

func (d *dashboard) confirm(prompt string, yes func()) {
	modal := tview.NewModal().
		SetText(prompt).
		AddButtons([]string{"Cancel", "Yes"}).
		SetDoneFunc(func(_ int, label string) {
			d.app.SetRoot(d.layout(), true).SetFocus(d.actions)
			if label == "Yes" {
				yes()
			}
		})
	d.app.SetRoot(modal, false).SetFocus(modal)
}

func (d *dashboard) moveAction(delta int) {
	current := d.actions.GetCurrentItem()
	next := current + delta
	if next < 0 {
		next = d.actions.GetItemCount() - 1
	}
	if next >= d.actions.GetItemCount() {
		next = 0
	}
	d.actions.SetCurrentItem(next)
}

func (d *dashboard) run() error {
	d.app.SetRoot(d.layout(), true).SetFocus(d.actions)
	d.refreshAsync()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(d.opts.Refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				d.refreshAsync()
			case <-stop:
				return
			}
		}
	}()

	return d.app.Run()
}

func (d *dashboard) layout() tview.Primitive {
	main := tview.NewFlex().
		AddItem(d.actions, 28, 0, true).
		AddItem(d.detail, 0, 1, false)

	return tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.header, 3, 0, false).
		AddItem(main, 0, 1, true).
		AddItem(d.footer, 1, 0, false)
}

func (d *dashboard) refreshAsync() {
	if !d.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.refreshing.Store(false)
		snap := collectDashboardSnapshot(d.opts, d.selectedLogs)
		d.app.QueueUpdateDraw(func() {
			d.header.SetText(tview.Escape(dashboardSummary(snap)))
			d.detail.SetText(dashboardDetails(snap))
			footer := " r refresh   q quit   arrows/j/k move   enter select "
			if d.statusMessage != "" {
				footer += "  " + tview.Escape(d.statusMessage)
			}
			d.footer.SetText(footer)
		})
	}()
}
