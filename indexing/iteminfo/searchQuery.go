package iteminfo

import (
	"strings"
)

// SearchOptions holds search parameters for name-based file/folder searching
type SearchOptions struct {
	CaseSensitive bool     // whether to perform case-sensitive matching
	Terms         []string // search terms to match
}

// ParseSearch parses a search query string into SearchOptions
// Supports:
// - case:exact for case-sensitive search
// - "quoted terms" for exact phrase matching
// - term1|term2 for multiple terms (OR logic)
func ParseSearch(value string) SearchOptions {
	opts := SearchOptions{
		CaseSensitive: strings.Contains(value, "case:exact"),
		Terms:         []string{},
	}

	// Remove the case:exact option from the value
	value = strings.ReplaceAll(value, "case:exact", "")
	value = strings.TrimSpace(value)

	if value == "" {
		return opts
	}

	// Handle quoted exact phrase search
	if value[0] == '"' && value[len(value)-1] == '"' {
		unique := strings.Trim(value, "\"")
		opts.Terms = []string{unique}
		return opts
	}

	// Split by | for multiple terms (OR logic)
	value = strings.TrimSpace(value)
	opts.Terms = strings.Split(value, "|")
	return opts
}
