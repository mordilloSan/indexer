package indexing

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/mordilloSan/go-logger/logger"
)

var (
	linuxSystemPaths = []string{"/proc", "/dev"}
	externalFSTypes  = map[string]struct{}{
		"nfs":        {},
		"nfs4":       {},
		"cifs":       {},
		"smbfs":      {},
		"smb2":       {},
		"smb3":       {},
		"fuse.cifs":  {},
		"fuse.smb":   {},
		"fuse.smb3":  {},
		"fuse.nfs":   {},
		"fuse.ceph":  {},
		"fuse.iscsi": {},
	}
	externalMountsOnce  sync.Once
	externalMountPoints map[string]string
)

func (idx *Index) isLinuxSystemPath(fullCombined string) bool {
	realPath := idx.realPathFromCombined(fullCombined)
	if realPath == "" {
		return false
	}

	for _, protectedPath := range linuxSystemPaths {
		protectedPath = filepath.Clean(protectedPath)
		if realPath == protectedPath {
			return true
		}
		if strings.HasPrefix(realPath, protectedPath+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func (idx *Index) isExternalMount(fullCombined string) bool {
	if runtime.GOOS != "linux" {
		return false
	}

	realPath := idx.realPathFromCombined(fullCombined)
	if realPath == "" {
		return false
	}

	mounts := getExternalMountPoints()
	if len(mounts) == 0 {
		return false
	}

	path := filepath.Clean(realPath)
	for mountPoint := range mounts {
		if mountPoint == "" || mountPoint == "/" {
			continue
		}
		if path == mountPoint || strings.HasPrefix(path, mountPoint+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

func getExternalMountPoints() map[string]string {
	externalMountsOnce.Do(func() {
		externalMountPoints = loadExternalMountPoints()
	})
	if externalMountPoints == nil {
		return map[string]string{}
	}
	return externalMountPoints
}

func loadExternalMountPoints() map[string]string {
	mounts := make(map[string]string)
	if runtime.GOOS != "linux" {
		return mounts
	}

	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		logger.Warnf("unable to read mountinfo: %v", err)
		return mounts
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		mountPoint, fsType, source, ok := parseMountInfo(scanner.Text())
		if !ok {
			continue
		}
		if isExternalFilesystem(fsType, source) {
			mounts[mountPoint] = fsType
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Warnf("error while scanning mountinfo: %v", err)
	}
	return mounts
}

func parseMountInfo(line string) (mountPoint, fsType, source string, ok bool) {
	parts := strings.Split(line, " - ")
	if len(parts) != 2 {
		return "", "", "", false
	}

	pre := strings.Fields(parts[0])
	post := strings.Fields(parts[1])
	if len(pre) < 5 || len(post) < 2 {
		return "", "", "", false
	}

	rawMountPoint := pre[4]
	mountPoint = decodeMountPath(rawMountPoint)
	mountPoint = filepath.Clean(mountPoint)
	fsType = strings.ToLower(post[0])
	source = strings.ToLower(post[1])
	return mountPoint, fsType, source, true
}

func decodeMountPath(raw string) string {
	decoded := strings.ReplaceAll(raw, "\\040", " ")
	decoded = strings.ReplaceAll(decoded, "\\011", "\t")
	decoded = strings.ReplaceAll(decoded, "\\012", "\n")
	decoded = strings.ReplaceAll(decoded, "\\134", "\\")
	return decoded
}

func isExternalFilesystem(fsType, source string) bool {
	if _, ok := externalFSTypes[fsType]; ok {
		return true
	}
	if strings.Contains(fsType, "iscsi") || strings.Contains(source, "iscsi") {
		return true
	}
	if strings.HasPrefix(source, "//") {
		return true
	}
	return false
}
