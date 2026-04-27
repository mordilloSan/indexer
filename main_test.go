package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnvFileQuotesAndReadsValuesWithSpaces(t *testing.T) {
	env := map[string]string{
		"INDEXER_PATH":        "/media/My Drive",
		"INDEXER_NAME":        "media drive",
		"INDEXER_DB_PATH":     `/tmp/indexer "main".db`,
		"INDEXER_LISTEN_FLAG": "--listen=:8080",
		"EXTRA_HASH":          "value#with#hashes",
	}

	content, err := formatEnvFile(env)
	if err != nil {
		t.Fatalf("formatEnvFile: %v", err)
	}
	if !strings.Contains(content, `INDEXER_PATH="/media/My Drive"`) {
		t.Fatalf("expected quoted path with spaces, got:\n%s", content)
	}
	if !strings.Contains(content, `INDEXER_DB_PATH="/tmp/indexer \"main\".db"`) {
		t.Fatalf("expected escaped quote in db path, got:\n%s", content)
	}
	if !strings.Contains(content, "INDEXER_LISTEN_FLAG=--listen=:8080") {
		t.Fatalf("expected simple listen flag to stay unquoted, got:\n%s", content)
	}

	path := filepath.Join(t.TempDir(), "indexer.env")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	got, err := readEnvFile(path)
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	for key, want := range env {
		if got[key] != want {
			t.Fatalf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestParseEnvLineAcceptsLegacyUnquotedSpaces(t *testing.T) {
	key, value, ok, err := parseEnvLine(`INDEXER_PATH=/media/My Drive`)
	if err != nil {
		t.Fatalf("parseEnvLine: %v", err)
	}
	if !ok || key != "INDEXER_PATH" || value != "/media/My Drive" {
		t.Fatalf("got key=%q value=%q ok=%t", key, value, ok)
	}
}

func TestFormatEnvFileUsesStableOrder(t *testing.T) {
	content, err := formatEnvFile(map[string]string{
		"ZZZ":          "last",
		"INDEXER_NAME": "root",
		"AAA":          "first",
		"INDEXER_PATH": "/",
	})
	if err != nil {
		t.Fatalf("formatEnvFile: %v", err)
	}
	want := "INDEXER_PATH=/\nINDEXER_NAME=root\nAAA=first\nZZZ=last\n"
	if content != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}

func TestSetupConfigFromEnvParsesListenFlag(t *testing.T) {
	cfg := setupConfigFromEnv(map[string]string{
		"INDEXER_PATH":                   "/srv/files",
		"INDEXER_NAME":                   "files",
		"INDEXER_INCLUDE_HIDDEN":         "no",
		"INDEXER_INCLUDE_NETWORK_MOUNTS": "yes",
		"INDEXER_KEEP_INDEXES":           "3",
		"INDEXER_LISTEN_FLAG":            "--listen=:9090",
	})

	if cfg.IndexPath != "/srv/files" || cfg.IndexName != "files" {
		t.Fatalf("unexpected identity fields: %#v", cfg)
	}
	if cfg.IncludeHidden || !cfg.IncludeNetworkMounts {
		t.Fatalf("unexpected bool fields: %#v", cfg)
	}
	if cfg.KeepIndexes != 3 {
		t.Fatalf("KeepIndexes = %d, want 3", cfg.KeepIndexes)
	}
	if cfg.ListenAddr != ":9090" {
		t.Fatalf("ListenAddr = %q, want :9090", cfg.ListenAddr)
	}
}

func TestSetupConfigPreservesUnknownListenFlag(t *testing.T) {
	cfg := setupConfigFromEnv(map[string]string{
		"INDEXER_LISTEN_FLAG": "--future-flag=value",
	})
	if cfg.ListenAddr != "" {
		t.Fatalf("ListenAddr = %q, want empty", cfg.ListenAddr)
	}
	if cfg.ListenFlag != "--future-flag=value" {
		t.Fatalf("ListenFlag = %q, want preserved raw flag", cfg.ListenFlag)
	}
	env := setupConfigToEnv(cfg)
	if env["INDEXER_LISTEN_FLAG"] != "--future-flag=value" {
		t.Fatalf("INDEXER_LISTEN_FLAG = %q, want preserved raw flag", env["INDEXER_LISTEN_FLAG"])
	}
}

func TestValidateConfigUpdatesRejectsUnsafeServiceValues(t *testing.T) {
	tests := []map[string]string{
		{"INDEXER_PATH": ""},
		{"INDEXER_SOCKET": ""},
		{"INDEXER_INTERVAL": "daily"},
		{"INDEXER_KEEP_INDEXES": "-1"},
	}
	for _, updates := range tests {
		if err := validateConfigUpdates(updates); err == nil {
			t.Fatalf("validateConfigUpdates(%v) returned nil", updates)
		}
	}
}
