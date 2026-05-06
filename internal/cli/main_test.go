package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mordilloSan/indexer/internal/configfile"
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

func TestWriteEnvFileUsesDefaultModeForNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexer.env")

	if err := writeEnvFile(path, map[string]string{"INDEXER_PATH": "/srv"}); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0644); got != want {
		t.Fatalf("mode = %v, want %v", got, want)
	}
}

func TestWriteEnvFilePreservesExistingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexer.env")
	if err := os.WriteFile(path, []byte("OLD=value\n"), 0600); err != nil {
		t.Fatalf("write seed env file: %v", err)
	}

	if err := writeEnvFile(path, map[string]string{"INDEXER_PATH": "/srv"}); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat env file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0600); got != want {
		t.Fatalf("mode = %v, want %v", got, want)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	if string(content) != "INDEXER_PATH=/srv\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestWriteEnvFileFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.env")
	link := filepath.Join(dir, "indexer.env")
	if err := os.WriteFile(target, []byte("OLD=value\n"), 0600); err != nil {
		t.Fatalf("write seed env file: %v", err)
	}
	if err := os.Symlink("target.env", link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := writeEnvFile(link, map[string]string{"INDEXER_PATH": "/srv"}); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced instead of following target")
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target env file: %v", err)
	}
	if string(content) != "INDEXER_PATH=/srv\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestApplyIndexEnvOverrides(t *testing.T) {
	t.Setenv("INDEXER_FRESH", "false")
	t.Setenv("INDEXER_INCLUDE_NETWORK_MOUNTS", "yes")
	t.Setenv("INDEXER_KEEP_INDEXES", "4")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	_ = fs.Bool("fresh", true, "")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "")
	_ = fs.Int("keep-indexes", 0, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := indexOptions{
		FreshIndex:           true,
		IncludeNetworkMounts: false,
		KeepIndexes:          0,
	}
	applyIndexEnvOverrides(fs, &opts)

	if opts.FreshIndex {
		t.Fatalf("FreshIndex = true, want false")
	}
	if !opts.IncludeNetworkMounts {
		t.Fatalf("IncludeNetworkMounts = false, want true")
	}
	if opts.KeepIndexes != 4 {
		t.Fatalf("KeepIndexes = %d, want 4", opts.KeepIndexes)
	}
}

func TestApplyIndexEnvOverridesKeepsExplicitFlags(t *testing.T) {
	t.Setenv("INDEXER_FRESH", "false")
	t.Setenv("INDEXER_INCLUDE_NETWORK_MOUNTS", "yes")
	t.Setenv("INDEXER_KEEP_INDEXES", "4")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fresh := fs.Bool("fresh", true, "")
	includeNetworkMounts := fs.Bool("include-network-mounts", false, "")
	fs.BoolVar(includeNetworkMounts, "include-external-mounts", false, "")
	keepIndexes := fs.Int("keep-indexes", 0, "")
	if err := fs.Parse([]string{"--fresh=true", "--include-network-mounts=false", "--keep-indexes=2"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	opts := indexOptions{
		FreshIndex:           *fresh,
		IncludeNetworkMounts: *includeNetworkMounts,
		KeepIndexes:          *keepIndexes,
	}
	applyIndexEnvOverrides(fs, &opts)

	if !opts.FreshIndex {
		t.Fatalf("FreshIndex = false, want true")
	}
	if opts.IncludeNetworkMounts {
		t.Fatalf("IncludeNetworkMounts = true, want false")
	}
	if opts.KeepIndexes != 2 {
		t.Fatalf("KeepIndexes = %d, want 2", opts.KeepIndexes)
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

func TestConfigPatchValidationRejectsUnsafeValues(t *testing.T) {
	empty := ""
	badInterval := "daily"
	negative := -1
	tests := []configfile.Patch{
		{IndexPath: &empty},
		{DBPath: &empty},
		{Interval: &badInterval},
		{KeepIndexes: &negative},
	}
	for _, patch := range tests {
		if _, err := configfile.ApplyPatch(configfile.Defaults(), patch); err == nil {
			t.Fatalf("ApplyPatch(%#v) returned nil", patch)
		}
	}
}
