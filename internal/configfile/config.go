package configfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mordilloSan/indexer/storage"
)

const (
	EnvConfigFile      = "INDEXER_CONFIG_FILE"
	NativeDefaultPath  = "/etc/indexer/config.json"
	LinuxIODefaultPath = "/config/indexer.json"
)

// Config is the stable JSON configuration shape exposed by the admin API and
// persisted on disk.
type Config struct {
	IndexPath            string `json:"index_path"`
	IndexName            string `json:"index_name"`
	IncludeHidden        bool   `json:"include_hidden"`
	IncludeNetworkMounts bool   `json:"include_network_mounts"`
	FreshIndex           bool   `json:"fresh_index"`
	KeepIndexes          int    `json:"keep_indexes"`
	DBPath               string `json:"db_path"`
	DBBusyTimeout        string `json:"db_busy_timeout"`
	DBJournalMode        string `json:"db_journal_mode"`
	DBSynchronous        string `json:"db_synchronous"`
	DBAutoVacuum         string `json:"db_auto_vacuum"`
	DBMaxOpenConns       int    `json:"db_max_open_conns"`
	DBMaxIdleConns       int    `json:"db_max_idle_conns"`
	DBConnMaxIdleTime    string `json:"db_conn_max_idle_time"`
	SocketPath           string `json:"socket_path"`
	ListenAddr           string `json:"listen_addr"`
	Interval             string `json:"interval"`
}

// Patch is used for config-file loading and PUT /config. Pointer fields allow
// false, 0, and empty strings to be intentional values.
type Patch struct {
	IndexPath            *string `json:"index_path"`
	IndexName            *string `json:"index_name"`
	IncludeHidden        *bool   `json:"include_hidden"`
	IncludeNetworkMounts *bool   `json:"include_network_mounts"`
	FreshIndex           *bool   `json:"fresh_index"`
	KeepIndexes          *int    `json:"keep_indexes"`
	DBPath               *string `json:"db_path"`
	DBBusyTimeout        *string `json:"db_busy_timeout"`
	DBJournalMode        *string `json:"db_journal_mode"`
	DBSynchronous        *string `json:"db_synchronous"`
	DBAutoVacuum         *string `json:"db_auto_vacuum"`
	DBMaxOpenConns       *int    `json:"db_max_open_conns"`
	DBMaxIdleConns       *int    `json:"db_max_idle_conns"`
	DBConnMaxIdleTime    *string `json:"db_conn_max_idle_time"`
	SocketPath           *string `json:"socket_path"`
	ListenAddr           *string `json:"listen_addr"`
	Interval             *string `json:"interval"`
}

func Defaults() Config {
	dbOpts := storage.DefaultOpenOptions()
	return Config{
		IndexPath:            "/",
		IndexName:            "root",
		IncludeHidden:        true,
		IncludeNetworkMounts: false,
		FreshIndex:           true,
		KeepIndexes:          0,
		DBPath:               "/tmp/indexer.db",
		DBBusyTimeout:        dbOpts.BusyTimeout.String(),
		DBJournalMode:        dbOpts.JournalMode,
		DBSynchronous:        dbOpts.Synchronous,
		DBAutoVacuum:         dbOpts.AutoVacuum,
		DBMaxOpenConns:       dbOpts.MaxOpenConns,
		DBMaxIdleConns:       dbOpts.MaxIdleConns,
		DBConnMaxIdleTime:    dbOpts.ConnMaxIdleTime.String(),
		SocketPath:           "/var/run/indexer.sock",
		ListenAddr:           "",
		Interval:             "1h0m0s",
	}
}

func DefaultPath() string {
	if info, err := os.Stat(filepath.Dir(LinuxIODefaultPath)); err == nil && info.IsDir() {
		return LinuxIODefaultPath
	}
	return NativeDefaultPath
}

func PathFromEnvOrDefault() string {
	if path := strings.TrimSpace(os.Getenv(EnvConfigFile)); path != "" {
		return path
	}
	return DefaultPath()
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	path = strings.TrimSpace(path)
	if path == "" || path == "-" {
		return Normalize(cfg)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Normalize(cfg)
		}
		return Config{}, fmt.Errorf("read config file %s: %w", path, err)
	}

	patch, err := DecodePatch(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
	}
	return ApplyPatch(cfg, patch)
}

func DecodePatch(data []byte) (Patch, error) {
	var patch Patch
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		return Patch{}, err
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Patch{}, fmt.Errorf("unexpected trailing JSON")
	}
	return patch, nil
}

func ApplyPatch(cfg Config, patch Patch) (Config, error) {
	if patch.IndexPath != nil {
		cfg.IndexPath = *patch.IndexPath
	}
	if patch.IndexName != nil {
		cfg.IndexName = *patch.IndexName
	}
	if patch.IncludeHidden != nil {
		cfg.IncludeHidden = *patch.IncludeHidden
	}
	if patch.IncludeNetworkMounts != nil {
		cfg.IncludeNetworkMounts = *patch.IncludeNetworkMounts
	}
	if patch.FreshIndex != nil {
		cfg.FreshIndex = *patch.FreshIndex
	}
	if patch.KeepIndexes != nil {
		cfg.KeepIndexes = *patch.KeepIndexes
	}
	if patch.DBPath != nil {
		cfg.DBPath = *patch.DBPath
	}
	if patch.DBBusyTimeout != nil {
		cfg.DBBusyTimeout = *patch.DBBusyTimeout
	}
	if patch.DBJournalMode != nil {
		cfg.DBJournalMode = *patch.DBJournalMode
	}
	if patch.DBSynchronous != nil {
		cfg.DBSynchronous = *patch.DBSynchronous
	}
	if patch.DBAutoVacuum != nil {
		cfg.DBAutoVacuum = *patch.DBAutoVacuum
	}
	if patch.DBMaxOpenConns != nil {
		cfg.DBMaxOpenConns = *patch.DBMaxOpenConns
	}
	if patch.DBMaxIdleConns != nil {
		cfg.DBMaxIdleConns = *patch.DBMaxIdleConns
	}
	if patch.DBConnMaxIdleTime != nil {
		cfg.DBConnMaxIdleTime = *patch.DBConnMaxIdleTime
	}
	if patch.SocketPath != nil {
		cfg.SocketPath = *patch.SocketPath
	}
	if patch.ListenAddr != nil {
		cfg.ListenAddr = *patch.ListenAddr
	}
	if patch.Interval != nil {
		cfg.Interval = *patch.Interval
	}
	return Normalize(cfg)
}

func Normalize(cfg Config) (Config, error) {
	cfg.IndexPath = strings.TrimSpace(cfg.IndexPath)
	if cfg.IndexPath == "" {
		return Config{}, fmt.Errorf("index_path cannot be empty")
	}

	cfg.IndexName = strings.TrimSpace(cfg.IndexName)
	if cfg.IndexName == "" {
		cfg.IndexName = DeriveIndexName(cfg.IndexPath)
	}

	cfg.DBPath = strings.TrimSpace(cfg.DBPath)
	if cfg.DBPath == "" {
		return Config{}, fmt.Errorf("db_path cannot be empty")
	}
	dbOpts, err := DBOpenOptions(cfg)
	if err != nil {
		return Config{}, err
	}
	cfg.DBBusyTimeout = dbOpts.BusyTimeout.String()
	cfg.DBJournalMode = dbOpts.JournalMode
	cfg.DBSynchronous = dbOpts.Synchronous
	cfg.DBAutoVacuum = dbOpts.AutoVacuum
	cfg.DBMaxOpenConns = dbOpts.MaxOpenConns
	cfg.DBMaxIdleConns = dbOpts.MaxIdleConns
	cfg.DBConnMaxIdleTime = dbOpts.ConnMaxIdleTime.String()

	cfg.SocketPath = strings.TrimSpace(cfg.SocketPath)
	if cfg.SocketPath == "-" {
		cfg.SocketPath = ""
	}
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)

	if cfg.KeepIndexes < 0 {
		return Config{}, fmt.Errorf("keep_indexes must be non-negative")
	}

	interval, _, err := NormalizeInterval(cfg.Interval)
	if err != nil {
		return Config{}, err
	}
	cfg.Interval = interval
	return cfg, nil
}

func DeriveIndexName(path string) string {
	name := strings.ReplaceAll(path, "/", "_")
	name = strings.Trim(name, "_")
	if name == "" {
		return "root"
	}
	return name
}

func NormalizeInterval(raw string) (string, time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return "0s", 0, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return "", 0, fmt.Errorf("invalid interval %q: %w", raw, err)
	}
	if interval < 0 {
		return "", 0, fmt.Errorf("interval must be non-negative")
	}
	return interval.String(), interval, nil
}

func ParseInterval(raw string) (time.Duration, error) {
	_, interval, err := NormalizeInterval(raw)
	return interval, err
}

func DBOpenOptions(cfg Config) (storage.OpenOptions, error) {
	defaults := storage.DefaultOpenOptions()
	busyTimeout, err := parseDurationWithDefault(cfg.DBBusyTimeout, defaults.BusyTimeout, "db_busy_timeout")
	if err != nil {
		return storage.OpenOptions{}, err
	}
	connMaxIdleTime, err := parseDurationWithDefault(cfg.DBConnMaxIdleTime, defaults.ConnMaxIdleTime, "db_conn_max_idle_time")
	if err != nil {
		return storage.OpenOptions{}, err
	}

	opts := storage.OpenOptions{
		BusyTimeout:     busyTimeout,
		JournalMode:     cfg.DBJournalMode,
		Synchronous:     cfg.DBSynchronous,
		AutoVacuum:      cfg.DBAutoVacuum,
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxIdleTime: connMaxIdleTime,
	}
	return storage.NormalizeOpenOptions(opts)
}

func parseDurationWithDefault(raw string, fallback time.Duration, field string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s must be non-negative", field)
	}
	return duration, nil
}

func ApplyEnvOverrides(cfg Config, lookup func(string) (string, bool)) (Config, error) {
	var patch Patch
	applyStringEnvOverrides(&patch, lookup)
	if err := applyBoolEnvOverrides(&patch, lookup); err != nil {
		return Config{}, err
	}
	if err := applyIntEnvOverrides(&patch, lookup); err != nil {
		return Config{}, err
	}
	if err := applyListenEnvOverride(&patch, lookup); err != nil {
		return Config{}, err
	}
	return ApplyPatch(cfg, patch)
}

type stringEnvOverride struct {
	key string
	set func(*Patch, *string)
}

func applyStringEnvOverrides(patch *Patch, lookup func(string) (string, bool)) {
	overrides := []stringEnvOverride{
		{"INDEXER_PATH", func(p *Patch, v *string) { p.IndexPath = v }},
		{"INDEXER_NAME", func(p *Patch, v *string) { p.IndexName = v }},
		{"INDEXER_DB_PATH", func(p *Patch, v *string) { p.DBPath = v }},
		{"INDEXER_DB_BUSY_TIMEOUT", func(p *Patch, v *string) { p.DBBusyTimeout = v }},
		{"INDEXER_DB_JOURNAL_MODE", func(p *Patch, v *string) { p.DBJournalMode = v }},
		{"INDEXER_DB_SYNCHRONOUS", func(p *Patch, v *string) { p.DBSynchronous = v }},
		{"INDEXER_DB_AUTO_VACUUM", func(p *Patch, v *string) { p.DBAutoVacuum = v }},
		{"INDEXER_DB_CONN_MAX_IDLE_TIME", func(p *Patch, v *string) { p.DBConnMaxIdleTime = v }},
		{"INDEXER_SOCKET", func(p *Patch, v *string) { p.SocketPath = v }},
		{"INDEXER_INTERVAL", func(p *Patch, v *string) { p.Interval = v }},
	}
	for _, override := range overrides {
		if value, ok := lookup(override.key); ok {
			override.set(patch, &value)
		}
	}
}

type boolEnvOverride struct {
	key string
	set func(*Patch, *bool)
}

func applyBoolEnvOverrides(patch *Patch, lookup func(string) (string, bool)) error {
	overrides := []boolEnvOverride{
		{"INDEXER_INCLUDE_HIDDEN", func(p *Patch, v *bool) { p.IncludeHidden = v }},
		{"INDEXER_FRESH", func(p *Patch, v *bool) { p.FreshIndex = v }},
	}
	for _, override := range overrides {
		if err := applyBoolEnvOverride(patch, lookup, override); err != nil {
			return err
		}
	}
	return applyNetworkMountsEnvOverride(patch, lookup)
}

func applyBoolEnvOverride(patch *Patch, lookup func(string) (string, bool), override boolEnvOverride) error {
	value, ok := lookup(override.key)
	if !ok {
		return nil
	}
	parsed, err := parseBoolEnv(override.key, value)
	if err != nil {
		return err
	}
	override.set(patch, &parsed)
	return nil
}

func applyNetworkMountsEnvOverride(patch *Patch, lookup func(string) (string, bool)) error {
	key := "INDEXER_INCLUDE_NETWORK_MOUNTS"
	value, ok := lookup(key)
	if !ok {
		key = "INDEXER_INCLUDE_EXTERNAL_MOUNTS"
		value, ok = lookup(key)
	}
	if !ok {
		return nil
	}
	parsed, err := parseBoolEnv(key, value)
	if err != nil {
		return err
	}
	patch.IncludeNetworkMounts = &parsed
	return nil
}

type intEnvOverride struct {
	key string
	set func(*Patch, *int)
}

func applyIntEnvOverrides(patch *Patch, lookup func(string) (string, bool)) error {
	overrides := []intEnvOverride{
		{"INDEXER_KEEP_INDEXES", func(p *Patch, v *int) { p.KeepIndexes = v }},
		{"INDEXER_DB_MAX_OPEN_CONNS", func(p *Patch, v *int) { p.DBMaxOpenConns = v }},
		{"INDEXER_DB_MAX_IDLE_CONNS", func(p *Patch, v *int) { p.DBMaxIdleConns = v }},
	}
	for _, override := range overrides {
		value, ok := lookup(override.key)
		if !ok {
			continue
		}
		parsed, err := parseNonNegativeIntEnv(override.key, value)
		if err != nil {
			return err
		}
		override.set(patch, &parsed)
	}
	return nil
}

func parseNonNegativeIntEnv(key, raw string) (int, error) {
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid %s %q: must be a non-negative integer", key, raw)
	}
	return parsed, nil
}

func applyListenEnvOverride(patch *Patch, lookup func(string) (string, bool)) error {
	if value, ok := lookup("INDEXER_LISTEN_ADDR"); ok {
		patch.ListenAddr = &value
		return nil
	}
	value, ok := lookup("INDEXER_LISTEN_FLAG")
	if !ok {
		return nil
	}
	listen, err := parseListenFlag(value)
	if err != nil {
		return err
	}
	patch.ListenAddr = &listen
	return nil
}

func Save(path string, cfg Config) error {
	cfg, err := Normalize(cfg)
	if err != nil {
		return err
	}
	data, err := Format(cfg)
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" || path == "-" {
		return fmt.Errorf("config file path is disabled")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return writeFileAtomic(path, data, 0o644)
}

func Format(cfg Config) ([]byte, error) {
	cfg, err := Normalize(cfg)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func parseBoolEnv(name, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid %s %q: must be true or false", name, raw)
	}
}

func parseListenFlag(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if after, ok := strings.CutPrefix(raw, "--listen="); ok {
		return strings.TrimSpace(after), nil
	}
	parts := strings.Fields(raw)
	if len(parts) == 2 && parts[0] == "--listen" {
		return strings.TrimSpace(parts[1]), nil
	}
	return "", fmt.Errorf("unsupported INDEXER_LISTEN_FLAG %q", raw)
}

func writeFileAtomic(path string, data []byte, defaultMode os.FileMode) (err error) {
	path, err = resolveAtomicWritePath(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	attrs, err := atomicWriteAttrs(path, defaultMode)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if err := prepareTempFile(tmp, tmpName, attrs, data); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file %s to %s: %w", tmpName, path, err)
	}
	removeTmp = false

	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

type atomicFileAttrs struct {
	mode os.FileMode
	uid  int
	gid  int
}

func resolveAtomicWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return path, nil
		}
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}
	return target, nil
}

func atomicWriteAttrs(path string, defaultMode os.FileMode) (atomicFileAttrs, error) {
	attrs := atomicFileAttrs{mode: defaultMode, uid: -1, gid: -1}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return attrs, nil
		}
		return attrs, fmt.Errorf("stat %s: %w", path, err)
	}
	attrs.mode = info.Mode().Perm()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		attrs.uid = int(stat.Uid)
		attrs.gid = int(stat.Gid)
	}
	return attrs, nil
}

func prepareTempFile(tmp *os.File, tmpName string, attrs atomicFileAttrs, data []byte) error {
	if err := tmp.Chmod(attrs.mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if attrs.uid >= 0 && attrs.gid >= 0 {
		if err := tmp.Chown(attrs.uid, attrs.gid); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("chown temp file %s: %w", tmpName, err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	return nil
}

func syncDir(dir string) (err error) {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}
