package cmd

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"github.com/mordilloSan/indexer/internal/configfile"
	"github.com/mordilloSan/indexer/storage"
)

type connectionKind string

const (
	connectionKindUnix connectionKind = "unix"
	connectionKindTCP  connectionKind = "tcp"
)

type connectionKindContextKey struct{}

func unixConnContext(ctx context.Context, _ net.Conn) context.Context {
	return withConnectionKind(ctx, connectionKindUnix)
}

func tcpConnContext(ctx context.Context, _ net.Conn) context.Context {
	return withConnectionKind(ctx, connectionKindTCP)
}

func withConnectionKind(ctx context.Context, kind connectionKind) context.Context {
	return context.WithValue(ctx, connectionKindContextKey{}, kind)
}

func requestFromUnixSocket(r *http.Request) bool {
	kind, _ := r.Context().Value(connectionKindContextKey{}).(connectionKind)
	return kind == connectionKindUnix
}

func daemonConfigToFileConfig(cfg DaemonConfig) (configfile.Config, error) {
	if cfg.DBOptions == (storage.OpenOptions{}) {
		cfg.DBOptions = storage.DefaultOpenOptions()
	}
	dbOptions, err := storage.NormalizeOpenOptions(cfg.DBOptions)
	if err != nil {
		return configfile.Config{}, err
	}
	interval := cfg.Interval.String()
	if cfg.Interval == 0 {
		interval = "0s"
	}
	return configfile.Normalize(configfile.Config{
		IndexPath:            cfg.IndexPath,
		IndexName:            cfg.IndexName,
		IncludeHidden:        cfg.IncludeHidden,
		IncludeNetworkMounts: cfg.IncludeNetworkMounts,
		FreshIndex:           cfg.FreshIndex,
		KeepIndexes:          cfg.KeepIndexes,
		DBPath:               cfg.DBPath,
		DBBusyTimeout:        dbOptions.BusyTimeout.String(),
		DBJournalMode:        dbOptions.JournalMode,
		DBSynchronous:        dbOptions.Synchronous,
		DBAutoVacuum:         dbOptions.AutoVacuum,
		DBMaxOpenConns:       dbOptions.MaxOpenConns,
		DBMaxIdleConns:       dbOptions.MaxIdleConns,
		DBConnMaxIdleTime:    dbOptions.ConnMaxIdleTime.String(),
		SocketPath:           cfg.SocketPath,
		ListenAddr:           cfg.ListenAddr,
		Interval:             interval,
	})
}

func (d *daemon) configSnapshot() DaemonConfig {
	d.cfgMu.RLock()
	defer d.cfgMu.RUnlock()
	return d.cfg
}

func (d *daemon) savedConfigSnapshot() configfile.Config {
	d.cfgMu.RLock()
	saved := d.savedConfig
	active := d.cfg
	d.cfgMu.RUnlock()
	if saved.IndexPath != "" {
		return saved
	}
	cfg, err := daemonConfigToFileConfig(active)
	if err != nil {
		return configfile.Defaults()
	}
	return cfg
}

func (d *daemon) applySavedConfig(saved configfile.Config) (bool, error) {
	next, err := DaemonConfigFromConfig(saved, d.configSnapshot().ConfigPath)
	if err != nil {
		return false, err
	}

	d.cfgMu.Lock()
	old := d.cfg
	restartRequired := false

	d.savedConfig = saved
	d.cfg.IndexName = next.IndexName
	d.cfg.IndexPath = next.IndexPath
	d.cfg.IncludeHidden = next.IncludeHidden
	d.cfg.IncludeNetworkMounts = next.IncludeNetworkMounts
	d.cfg.FreshIndex = next.FreshIndex
	d.cfg.KeepIndexes = next.KeepIndexes
	d.cfg.Interval = next.Interval

	if old.DBOptions == next.DBOptions {
		d.cfg.DBOptions = next.DBOptions
	} else {
		restartRequired = true
	}
	if old.DBPath == next.DBPath {
		d.cfg.DBPath = next.DBPath
	} else {
		restartRequired = true
	}
	if old.SocketPath == next.SocketPath {
		d.cfg.SocketPath = next.SocketPath
	} else {
		restartRequired = true
	}
	if old.ListenAddr == next.ListenAddr {
		d.cfg.ListenAddr = next.ListenAddr
	} else {
		restartRequired = true
	}
	d.cfg.ConfigPath = old.ConfigPath
	intervalChanged := old.Interval != d.cfg.Interval
	d.cfgMu.Unlock()

	if intervalChanged {
		d.notifyConfigChanged()
	}
	return restartRequired, nil
}

func (d *daemon) notifyConfigChanged() {
	if d.configChanged == nil {
		return
	}
	select {
	case d.configChanged <- struct{}{}:
	default:
	}
}

func appendDBOptionArgs(args []string, opts storage.OpenOptions) []string {
	opts, err := storage.NormalizeOpenOptions(opts)
	if err != nil {
		opts = storage.DefaultOpenOptions()
	}
	return append(args,
		"--db-busy-timeout", opts.BusyTimeout.String(),
		"--db-journal-mode", opts.JournalMode,
		"--db-synchronous", opts.Synchronous,
		"--db-auto-vacuum", opts.AutoVacuum,
		"--db-max-open-conns", strconv.Itoa(opts.MaxOpenConns),
		"--db-max-idle-conns", strconv.Itoa(opts.MaxIdleConns),
		"--db-conn-max-idle-time", opts.ConnMaxIdleTime.String(),
	)
}
