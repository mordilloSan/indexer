package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
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

func appendf(b *strings.Builder, format string, args ...any) {
	if _, err := fmt.Fprintf(b, format, args...); err != nil {
		panic(fmt.Errorf("append formatted text: %w", err))
	}
}

func writeOrExit(w io.Writer, args ...any) {
	if _, err := fmt.Fprint(w, args...); err != nil {
		handleCLIWriteError(err)
	}
}

func writefOrExit(w io.Writer, format string, args ...any) {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		handleCLIWriteError(err)
	}
}

func writelnOrExit(w io.Writer, args ...any) {
	if _, err := fmt.Fprintln(w, args...); err != nil {
		handleCLIWriteError(err)
	}
}

func handleCLIWriteError(err error) {
	if errors.Is(err, syscall.EPIPE) {
		os.Exit(1)
	}

	message := "indexer: write output failed: " + err.Error() + "\n"
	if _, reportErr := os.Stderr.WriteString(message); reportErr != nil {
		slog.Error("failed to write CLI output and error report", "err", err, "report_err", reportErr)
	}
	os.Exit(1)
}

func parseInterval(s string) (time.Duration, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
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
	v, ok := parseBool(os.Getenv(name))
	return ok && v
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
