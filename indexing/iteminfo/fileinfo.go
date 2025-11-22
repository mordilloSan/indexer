package iteminfo

import (
	"time"
)

// ItemInfo represents basic information about a file or directory
type ItemInfo struct {
	Name    string    `json:"name"`     // name of the file/folder
	Size    int64     `json:"size"`     // size in bytes
	ModTime time.Time `json:"modified"` // modification time
	Type    string    `json:"type"`     // "directory" for folders, or basic type for files
	Hidden  bool      `json:"hidden"`   // whether the file is hidden
	Inode   uint64    `json:"inode"`    // inode identifier when available
}

// FileInfo describes a file or directory with its contents
type FileInfo struct {
	ItemInfo
	Files   []ItemInfo `json:"files"`   // files in the directory
	Folders []ItemInfo `json:"folders"` // folders in the directory
	Path    string     `json:"path"`    // path scoped to the associated index
}

// ExtendedFileInfo provides additional metadata for API responses
type ExtendedFileInfo struct {
	FileInfo
	Content   string            `json:"content,omitempty"`   // text content of a file, if requested
	Checksums map[string]string `json:"checksums,omitempty"` // checksums for the file
	Source    string            `json:"source,omitempty"`    // associated index source for the file
	RealPath  string            `json:"-"`
}
