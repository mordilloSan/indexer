package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// readEnvFile parses the subset of systemd EnvironmentFile syntax used by the
// indexer service: one KEY=VALUE assignment per line, with quoted values when
// whitespace or shell-significant characters are needed.
func readEnvFile(path string) (_ map[string]string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return map[string]string{}, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	result := map[string]string{}
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok, err := parseEnvLine(scanner.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if !ok {
			continue
		}
		result[key] = value
	}
	return result, scanner.Err()
}

// writeEnvFile writes env vars in canonical key order to path.
func writeEnvFile(path string, env map[string]string) error {
	content, err := formatEnvFile(env)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(content), 0644)
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
		if os.IsNotExist(err) {
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
		if os.IsNotExist(err) {
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

func formatEnvFile(env map[string]string) (string, error) {
	order := []string{
		"INDEXER_PATH", "INDEXER_NAME", "INDEXER_INCLUDE_HIDDEN",
		"INDEXER_INCLUDE_NETWORK_MOUNTS", "INDEXER_FRESH",
		"INDEXER_KEEP_INDEXES", "INDEXER_DB_PATH",
		"INDEXER_SOCKET", "INDEXER_INTERVAL", "INDEXER_LISTEN_FLAG",
	}
	inOrder := map[string]bool{}
	for _, k := range order {
		inOrder[k] = true
	}

	var sb strings.Builder
	for _, k := range order {
		if v, ok := env[k]; ok {
			line, err := formatEnvAssignment(k, v)
			if err != nil {
				return "", err
			}
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	var extraKeys []string
	for k := range env {
		if !inOrder[k] {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		line, err := formatEnvAssignment(k, env[k])
		if err != nil {
			return "", err
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

func parseEnvLine(line string) (key, value string, ok bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
		return "", "", false, nil
	}

	keyPart, valuePart, found := strings.Cut(line, "=")
	if !found {
		return "", "", false, fmt.Errorf("expected KEY=VALUE assignment")
	}
	key = strings.TrimSpace(keyPart)
	if !isValidEnvKey(key) {
		return "", "", false, fmt.Errorf("invalid environment key %q", key)
	}
	value, err = parseEnvValue(strings.TrimSpace(valuePart))
	if err != nil {
		return "", "", false, err
	}
	return key, value, true, nil
}

func parseEnvValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	switch raw[0] {
	case '\'':
		return parseSingleQuotedEnvValue(raw)
	case '"':
		return parseDoubleQuotedEnvValue(raw)
	default:
		return parseUnquotedEnvValue(raw)
	}
}

func parseSingleQuotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	for i := 1; i < len(raw); i++ {
		if raw[i] == '\'' {
			if err := validateEnvTrailingText(raw[i+1:]); err != nil {
				return "", err
			}
			return sb.String(), nil
		}
		sb.WriteByte(raw[i])
	}
	return "", fmt.Errorf("unterminated single-quoted value")
}

func parseDoubleQuotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	escaped := false
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			switch ch {
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			default:
				sb.WriteByte(ch)
			}
			escaped = false
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '"':
			if err := validateEnvTrailingText(raw[i+1:]); err != nil {
				return "", err
			}
			return sb.String(), nil
		default:
			sb.WriteByte(ch)
		}
	}
	if escaped {
		return "", fmt.Errorf("unterminated escape in double-quoted value")
	}
	return "", fmt.Errorf("unterminated double-quoted value")
}

func parseUnquotedEnvValue(raw string) (string, error) {
	var sb strings.Builder
	escaped := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			sb.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		sb.WriteByte(ch)
	}
	if escaped {
		return "", fmt.Errorf("unterminated escape in unquoted value")
	}
	return strings.TrimSpace(sb.String()), nil
}

func validateEnvTrailingText(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, ";") {
		return nil
	}
	return fmt.Errorf("unexpected text after quoted value")
}

func formatEnvAssignment(key, value string) (string, error) {
	if !isValidEnvKey(key) {
		return "", fmt.Errorf("invalid environment key %q", key)
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%s contains a newline or NUL byte", key)
	}
	return key + "=" + quoteEnvValue(value), nil
}

func quoteEnvValue(value string) string {
	if value == "" {
		return ""
	}
	if isSafeEnvValue(value) {
		return value
	}

	var sb strings.Builder
	sb.WriteByte('"')
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '\\', '"':
			sb.WriteByte('\\')
			sb.WriteByte(value[i])
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteByte(value[i])
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func isSafeEnvValue(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			continue
		}
		switch ch {
		case '/', '.', '_', '-', ':', '@', '%', '+', '=', ',':
			continue
		default:
			return false
		}
	}
	return true
}

func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		ch := key[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '_' {
			continue
		}
		if i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return false
	}
	return true
}
