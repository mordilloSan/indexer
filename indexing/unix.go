package indexing

import (
	"strings"
	"syscall"
)

func getFileDetails(sys any) (uint64, uint64, uint64, bool) {
	if stat, ok := sys.(*syscall.Stat_t); ok {
		// Use allocated size for `du`-like behavior
		realSize := uint64(stat.Blocks * 512)
		return realSize, uint64(stat.Nlink), stat.Ino, true
	}
	return 0, 1, 0, false
}

// MakeIndexPathPlatform normalizes paths for Linux
func (idx *Index) MakeIndexPathPlatform(path string) string {
	return "/" + strings.TrimPrefix(path, "/")
}
