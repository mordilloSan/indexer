package cli

import (
	"flag"

	"github.com/mordilloSan/indexer/internal/configfile"
)

type daemonFlagValues struct {
	indexPath            *string
	indexName            *string
	includeHidden        *bool
	includeNetworkMounts *bool
	freshIndex           *bool
	keepIndexes          *int
	dbPath               *string
	dbBusyTimeout        *string
	dbJournalMode        *string
	dbSynchronous        *string
	dbAutoVacuum         *string
	dbMaxOpenConns       *int
	dbMaxIdleConns       *int
	dbConnMaxIdleTime    *string
	socketPath           *string
	listenAddr           *string
	interval             *string
}

func applyDaemonFlagOverrides(fs *flag.FlagSet, cfg configfile.Config, values daemonFlagValues) (configfile.Config, error) {
	var patch configfile.Patch
	if flagWasSet(fs, "path") {
		patch.IndexPath = values.indexPath
	}
	if flagWasSet(fs, "name") {
		patch.IndexName = values.indexName
	}
	if flagWasSet(fs, "include-hidden") {
		patch.IncludeHidden = values.includeHidden
	}
	if flagWasSet(fs, "include-network-mounts") || flagWasSet(fs, "include-external-mounts") {
		patch.IncludeNetworkMounts = values.includeNetworkMounts
	}
	if flagWasSet(fs, "fresh") {
		patch.FreshIndex = values.freshIndex
	}
	if flagWasSet(fs, "keep-indexes") {
		patch.KeepIndexes = values.keepIndexes
	}
	if flagWasSet(fs, "db-path") {
		patch.DBPath = values.dbPath
	}
	if flagWasSet(fs, "db-busy-timeout") {
		patch.DBBusyTimeout = values.dbBusyTimeout
	}
	if flagWasSet(fs, "db-journal-mode") {
		patch.DBJournalMode = values.dbJournalMode
	}
	if flagWasSet(fs, "db-synchronous") {
		patch.DBSynchronous = values.dbSynchronous
	}
	if flagWasSet(fs, "db-auto-vacuum") {
		patch.DBAutoVacuum = values.dbAutoVacuum
	}
	if flagWasSet(fs, "db-max-open-conns") {
		patch.DBMaxOpenConns = values.dbMaxOpenConns
	}
	if flagWasSet(fs, "db-max-idle-conns") {
		patch.DBMaxIdleConns = values.dbMaxIdleConns
	}
	if flagWasSet(fs, "db-conn-max-idle-time") {
		patch.DBConnMaxIdleTime = values.dbConnMaxIdleTime
	}
	if flagWasSet(fs, "socket-path") {
		patch.SocketPath = values.socketPath
	}
	if flagWasSet(fs, "listen") {
		patch.ListenAddr = values.listenAddr
	}
	if flagWasSet(fs, "interval") {
		patch.Interval = values.interval
	}
	return configfile.ApplyPatch(cfg, patch)
}
