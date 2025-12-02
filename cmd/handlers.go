package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/indexing"
)

func (d *daemon) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !d.tryLockIndex() {
		http.Error(w, "indexer already running", http.StatusConflict)
		return
	}
	go func() {
		defer d.unlockIndex()
		if err := d.runIndexSubprocess(context.Background()); err != nil {
			logger.Errorf("manual index failed: %v", err)
		}
	}()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"running"}`))
}

func (d *daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	running := d.running.Load()

	// Response shape (backwards-compatible + extra stats)
	var resp struct {
		Status       string `json:"status"`
		NumDirs      int64  `json:"num_dirs"`
		NumFiles     int64  `json:"num_files"`
		TotalSize    int64  `json:"total_size"`
		LastIndexed  string `json:"last_indexed"`
		TotalIndexes int    `json:"total_indexes"`
		TotalEntries int64  `json:"total_entries"`
		DatabaseSize int64  `json:"database_size"`
		Warning      string `json:"warning,omitempty"`
	}

	if running {
		resp.Status = "running"
	} else {
		resp.Status = "idle"
	}

	addWarning := func(msg string) {
		if resp.Warning == "" {
			resp.Warning = msg
		} else {
			resp.Warning += "; " + msg
		}
	}

	// 1) Per-latest-index stats (dirs/files/size/last_indexed)
	li, err := loadLatestIndex(ctx, d.db)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No prior index: leave NumDirs/NumFiles/TotalSize/LastIndexed as zero values
	case err != nil:
		if running {
			addWarning(fmt.Sprintf("latest index unavailable: %v", err))
			logger.Warnf("Status: latest index unavailable while indexing: %v", err)
		} else {
			http.Error(w, fmt.Sprintf("error loading status: %v", err), http.StatusInternalServerError)
			return
		}
	default:
		resp.NumDirs = li.NumDirs
		resp.NumFiles = li.NumFiles
		resp.TotalSize = li.TotalSize
		if li.LastIndexed.Valid && li.LastIndexed.Int64 > 0 {
			resp.LastIndexed = time.Unix(li.LastIndexed.Int64, 0).UTC().Format(time.RFC3339)
		}
	}

	// 2) Global stats (uses Store with correct dbPath for db size)
	stats, err := d.store.GetStats(ctx)
	if err != nil {
		if running {
			addWarning(fmt.Sprintf("stats unavailable: %v", err))
			logger.Warnf("Status: global stats unavailable while indexing: %v", err)
		} else {
			http.Error(w, fmt.Sprintf("error loading stats: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if stats != nil {
		resp.TotalIndexes = stats.TotalIndexes
		resp.TotalEntries = stats.TotalEntries
		resp.DatabaseSize = stats.DatabaseSize
	}

	writeJSON(w, resp)
}

func (d *daemon) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := queryInt(r.URL.Query().Get("limit"), 100, 1)
	results, err := d.store.SearchEntries(ctx, q, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := queryPathOrRoot(r.URL.Query().Get("path"))
	recursive := r.URL.Query().Get("recursive") == "true"
	limit := queryInt(r.URL.Query().Get("limit"), 200, 1)
	offset := queryInt(r.URL.Query().Get("offset"), 0, 0)

	results, err := d.store.QueryPath(ctx, path, recursive, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
}

func (d *daemon) handleDirSize(w http.ResponseWriter, r *http.Request) {
	path := queryPathOrRoot(r.URL.Query().Get("path"))
	total, err := d.store.DirSize(r.Context(), path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{
		"path": path,
		"size": total,
	})
}

func (d *daemon) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Path    string `json:"path"`
		AbsPath string `json:"absPath"`
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		IsDir   bool   `json:"isDir"`
		Type    string `json:"type"`
		Hidden  bool   `json:"hidden"`
		ModUnix int64  `json:"modUnix"`
		Inode   uint64 `json:"inode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid body: %v", err), http.StatusBadRequest)
		return
	}
	if payload.Path == "" || payload.Name == "" {
		http.Error(w, "path and name are required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("no index present: %v", err), http.StatusBadRequest)
		return
	}

	relPath := indexing.NormalizeIndexPath(payload.Path)
	absPath := payload.AbsPath
	if absPath == "" {
		absPath = payload.Path
	}
	modUnix := payload.ModUnix
	if modUnix == 0 {
		modUnix = time.Now().UTC().Unix()
	}
	entryType := payload.Type
	if entryType == "" {
		if payload.IsDir {
			entryType = "directory"
		} else {
			entryType = "file"
		}
	}

	entry := indexing.IndexEntry{
		RelativePath: relPath,
		AbsolutePath: absPath,
		Name:         payload.Name,
		Size:         payload.Size,
		ModTime:      time.Unix(modUnix, 0),
		Type:         entryType,
		Hidden:       payload.Hidden,
		IsDir:        payload.IsDir,
		Inode:        payload.Inode,
	}

	if err := d.store.UpsertEntryWithSizeUpdate(ctx, indexID, entry); err != nil {
		http.Error(w, fmt.Sprintf("upsert failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (d *daemon) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "use DELETE", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	indexID, err := d.store.LatestIndexID(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("no index present: %v", err), http.StatusBadRequest)
		return
	}

	relPath := indexing.NormalizeIndexPath(path)
	if err := d.store.DeleteEntryWithSizeUpdate(ctx, indexID, relPath); err != nil {
		http.Error(w, fmt.Sprintf("delete failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// queryInt parses an integer query parameter with default and minimum value.
func queryInt(q string, def int, min int) int {
	if q == "" {
		return def
	}
	v, err := strconv.Atoi(q)
	if err != nil || v < min {
		return def
	}
	return v
}

// queryPathOrRoot returns the path query parameter or "/" if empty.
func queryPathOrRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// Minimal OpenAPI spec served at /openapi.json.
func serveOpenapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(openapiSpec))
}

const openapiSpec = `{
  "openapi": "3.0.0",
  "info": { "title": "Indexer API", "version": "1.0.0" },
  "paths": {
    "/index": { "post": { "summary": "Trigger index", "responses": { "202": {"description": "Started"}, "409": {"description": "Already running"} } } },
    "/status": { "get": { "summary": "Get status", "responses": { "200": {"description": "Status"} } } },
    "/search": { "get": { "summary": "Search entries", "parameters": [{ "in": "query", "name": "q", "schema": {"type": "string"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Results"} } } },
    "/dirsize": { "get": { "summary": "Directory size", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "Size"} } } },
    "/entries": { "get": { "summary": "List entries", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }, { "in": "query", "name": "recursive", "schema": {"type": "boolean"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }, { "in": "query", "name": "offset", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Entries"} } } },
    "/add": { "post": { "summary": "Upsert entry", "responses": { "200": {"description": "OK"} } } },
    "/delete": { "delete": { "summary": "Delete entry", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "OK"} } } }
  }
}`
