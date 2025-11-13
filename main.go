package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mordilloSan/go_logger/logger"
	"indexer/indexing"
)

func main() {
	// Define command-line flags
	indexPath := flag.String("path", "", "Path to index (required, e.g., /, /home, /var)")
	indexName := flag.String("name", "", "Name for this index (optional, defaults to path)")
	searchTerm := flag.String("search", "", "Search term to find files/folders after indexing (optional)")
	caseSensitive := flag.Bool("case-sensitive", false, "Perform case-sensitive search")
	verbose := flag.Bool("verbose", false, "Enable verbose logging (DEBUG level)")
	includeHidden := flag.Bool("include-hidden", false, "Include hidden files and directories (starting with .)")

	flag.Parse()

	// Initialize logger (development mode with optional verbose debug)
	logger.Init("development", *verbose)

	// Validate required flags
	if *indexPath == "" {
		logger.Errorf("Error: -path flag is required")
		fmt.Println("\nUsage:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Set default name if not provided
	name := *indexName
	if name == "" {
		name = strings.ReplaceAll(*indexPath, "/", "_")
		if name == "" || name == "_" {
			name = "root"
		}
	}

	// Initialize the indexer
	logger.Infof("Initializing indexer for path: %s", *indexPath)
	index := indexing.Initialize(name, *indexPath, *indexPath, *includeHidden)

	// Start indexing with timer
	logger.Infof("Starting indexing...")
	startTime := time.Now()
	err := index.StartIndexing()
	duration := time.Since(startTime)

	if err != nil {
		logger.Errorf("Error during indexing: %v", err)
		os.Exit(1)
	}

	// Print summary
	logger.Infof("Indexing completed successfully!")
	logger.Infof("Total directories: %d", index.NumDirs)
	logger.Infof("Total files: %d", index.NumFiles)
	logger.Infof("Total size: %d bytes (%.2f GB)", index.GetTotalSize(), float64(index.GetTotalSize())/(1024*1024*1024))
	logger.Infof("Indexing duration: %v", duration)

	// Perform search if search term provided
	if *searchTerm != "" {
		logger.Infof("Searching for: %s", *searchTerm)
		results := index.Search(*searchTerm, *caseSensitive)

		if len(results) == 0 {
			logger.Infof("No matches found")
		} else {
			for _, result := range results {
				if result.IsDir {
					logger.Infof("  Folder: %s", result.Path)
				} else {
					logger.Infof("  File: %s (size: %d bytes)", result.Path, result.Size)
				}
			}
			logger.Infof("Total matches: %d", len(results))
		}
	}
}
