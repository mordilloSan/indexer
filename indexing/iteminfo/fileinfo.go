package iteminfo

import (
	"time"
)

type ItemInfo struct {
	Name       string    `json:"name"`       // name of the file
	Size       int64     `json:"size"`       // length in bytes for regular files
	ModTime    time.Time `json:"modified"`   // modification time
	Type       string    `json:"type"`       // type of the file, either "directory" or a file mimetype
	Hidden     bool      `json:"hidden"`     // whether the file is hidden
	HasPreview bool      `json:"hasPreview"` // whether the file has a thumbnail preview
}

// FileInfo describes a file.
// reduced item is non-recursive reduced "Items", used to pass flat items array
type FileInfo struct {
	ItemInfo
	Files   []ItemInfo `json:"files"`   // files in the directory
	Folders []ItemInfo `json:"folders"` // folders in the directory
	Path    string     `json:"path"`    // path scoped to the associated index
}

// for efficiency, a response will be a pointer to the data
// extra calculated fields can be added here
type ExtendedFileInfo struct {
	FileInfo
	Content      string            `json:"content,omitempty"`      // text content of a file, if requested
	Checksums    map[string]string `json:"checksums,omitempty"`    // checksums for the file
	Token        string            `json:"token,omitempty"`        // token for the file -- used for sharing
	OnlyOfficeId string            `json:"onlyOfficeId,omitempty"` // id for onlyoffice files
	Source       string            `json:"source,omitempty"`       // associated index source for the file
	Hash         string            `json:"hash,omitempty"`         // hash for the file -- used for sharing
	RealPath     string            `json:"-"`
}
