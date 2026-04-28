package cli

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

func parseBoolWithFallback(raw string, fallback bool) bool {
	if parsed, ok := parseBool(raw); ok {
		return parsed
	}
	return fallback
}

func parseBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true, true
	case "0", "f", "false", "n", "no", "off":
		return false, true
	default:
		return false, false
	}
}

func displayOptional(value, fallback string) string {
	if strings.TrimSpace(value) == "" || value == "-" && fallback == "disabled" {
		return fallback
	}
	return value
}

func sanitizeName(name, path string) string {
	if name != "" {
		return name
	}
	name = strings.ReplaceAll(path, "/", "_")
	if name == "" || name == "_" {
		return "root"
	}
	return strings.Trim(name, "_")
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseInterval(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

func configureLogger(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	wasSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			wasSet = true
		}
	})
	return wasSet
}

func envBool(name string) bool {
	v, _ := parseBool(os.Getenv(name))
	return v
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		slog.Warn("invalid integer environment value", "name", name, "value", raw, "err", err)
		return fallback
	}
	if v < 0 {
		slog.Warn("negative integer environment value ignored", "name", name, "value", raw)
		return fallback
	}
	return v
}

func ioReadAllLimit(r io.Reader, limit int64) ([]byte, error) {
	lr := &io.LimitedReader{R: r, N: limit}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if lr.N <= 0 {
		return nil, fmt.Errorf("response too large (>%d bytes)", limit)
	}
	return b, nil
}
