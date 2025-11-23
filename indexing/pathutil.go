package indexing

import "strings"

// NormalizeIndexPath returns the canonical relative path representation used throughout the index:
// always leading "/", no trailing "/" (except for root which stays "/").
func NormalizeIndexPath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	return "/" + strings.Trim(p, "/")
}

// BoolToInt returns 1 for true, 0 for false.
func BoolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
