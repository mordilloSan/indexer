package indexing

import (
	"os"
	"syscall"
)

func (idx *Index) handleFile(file os.FileInfo, fullCombined string) (size uint64, shouldCountSize bool) {
	var realSize uint64
	var nlink uint64 = 1
	var ino uint64 = 0
	canUseSyscall := false

	if sys := file.Sys(); sys != nil {
		realSize, nlink, ino, canUseSyscall = getFileDetails(sys)
	}

	if !canUseSyscall {
		// Fallback for non-unix systems or if syscall info is unavailable
		realSize = uint64(file.Size())
	}

	if nlink > 1 {
		idx.mu.Lock()
		if _, exists := idx.processedInodes[ino]; exists {
			idx.mu.Unlock()
			return realSize, false
		}
		idx.processedInodes[ino] = struct{}{}
		idx.FoundHardLinks[fullCombined] = realSize
		idx.totalSize += realSize
		idx.mu.Unlock()
		return realSize, true
	}

	// It's a regular file.
	idx.mu.Lock()
	idx.totalSize += realSize
	idx.mu.Unlock()
	return realSize, true // Count size.
}

func (idx *Index) incrementDirCount() {
	idx.mu.Lock()
	idx.NumDirs++
	idx.mu.Unlock()
}

func (idx *Index) incrementFileCount() {
	idx.mu.Lock()
	idx.NumFiles++
	idx.mu.Unlock()
}

func inodeFromFileInfo(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	_, _, ino, ok := getFileDetails(info.Sys())
	if ok {
		return ino
	}
	return 0
}

func getFileDetails(sys any) (uint64, uint64, uint64, bool) {
	if stat, ok := sys.(*syscall.Stat_t); ok {
		// Use allocated size for `du`-like behavior
		realSize := uint64(stat.Blocks * 512)
		return realSize, uint64(stat.Nlink), stat.Ino, true
	}
	return 0, 1, 0, false
}
