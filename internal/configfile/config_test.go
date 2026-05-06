package configfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IndexPath != "/" || cfg.IndexName != "root" || cfg.Interval != "1h0m0s" {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}
}

func TestLoadAppliesPartialConfigWithFalseValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexer.json")
	content := `{
  "index_path": "/srv/files",
  "index_name": "",
  "include_hidden": false,
  "fresh_index": false,
  "keep_indexes": 2,
  "interval": "30m"
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.IndexPath != "/srv/files" || cfg.IndexName != "srv_files" {
		t.Fatalf("unexpected identity fields: %#v", cfg)
	}
	if cfg.IncludeHidden || cfg.FreshIndex {
		t.Fatalf("false values were not preserved: %#v", cfg)
	}
	if cfg.KeepIndexes != 2 || cfg.Interval != "30m0s" {
		t.Fatalf("unexpected normalized fields: %#v", cfg)
	}
}

func TestSaveWritesNormalizedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "indexer.json")
	cfg := Defaults()
	cfg.Interval = "1h"

	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(content), `"interval": "1h0m0s"`) {
		t.Fatalf("expected normalized interval, got:\n%s", content)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	env := map[string]string{
		"INDEXER_PATH":                   "/data",
		"INDEXER_INCLUDE_HIDDEN":         "false",
		"INDEXER_INCLUDE_NETWORK_MOUNTS": "yes",
		"INDEXER_KEEP_INDEXES":           "3",
		"INDEXER_DB_SYNCHRONOUS":         "normal",
		"INDEXER_DB_MAX_OPEN_CONNS":      "7",
		"INDEXER_LISTEN_FLAG":            "--listen=:8080",
	}
	cfg, err := ApplyEnvOverrides(Defaults(), func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	})
	if err != nil {
		t.Fatalf("ApplyEnvOverrides: %v", err)
	}
	if cfg.IndexPath != "/data" || cfg.IncludeHidden || !cfg.IncludeNetworkMounts || cfg.KeepIndexes != 3 || cfg.DBSynchronous != "NORMAL" || cfg.DBMaxOpenConns != 7 || cfg.ListenAddr != ":8080" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestDBOpenOptionsValidation(t *testing.T) {
	cfg := Defaults()
	cfg.DBJournalMode = "WAL; DROP TABLE entries"
	if _, err := Normalize(cfg); err == nil {
		t.Fatalf("Normalize accepted invalid journal mode")
	}
}
