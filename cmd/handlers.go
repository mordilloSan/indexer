package cmd

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mordilloSan/go_logger/logger"

	"github.com/mordilloSan/indexer/indexing"
	"github.com/mordilloSan/indexer/storage"
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

func (d *daemon) handleReindex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	// Normalize the path
	normalizedPath := indexing.NormalizeIndexPath(path)

	if !d.tryLockIndex() {
		http.Error(w, "indexer already running", http.StatusConflict)
		return
	}

	go func() {
		defer d.unlockIndex()
		if err := d.reindexPath(context.Background(), normalizedPath); err != nil {
			logger.Errorf("reindex failed for path %s: %v", normalizedPath, err)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{
		"status": "running",
		"path":   normalizedPath,
	})
}

func (d *daemon) handleVacuum(w http.ResponseWriter, r *http.Request) {
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

		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
		defer cancel()

		indexID, err := d.store.LatestIndexID(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			logger.Warnf("vacuum: latest index id unavailable: %v", err)
			indexID = 0
		}

		if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
			logger.Warnf("vacuum: wal checkpoint (pre) failed: %v", err)
		}

		vs, err := storage.Vacuum(ctx, d.db)
		if err != nil {
			logger.Errorf("vacuum failed: %v", err)
			return
		}
		logger.Infof("Vacuum complete in %v", vs.Duration)

		if _, err := storage.WALCheckpointTruncate(ctx, d.db); err != nil {
			logger.Warnf("vacuum: wal checkpoint (post) failed: %v", err)
		}
		_ = storage.ReleaseSQLiteMemory(ctx, d.db)

		if indexID != 0 {
			if _, err := d.db.ExecContext(ctx, `UPDATE indexes SET vacuum_duration_ms = ? WHERE id = ?;`, vs.Duration.Milliseconds(), indexID); err != nil {
				logger.Warnf("vacuum: failed to persist duration: %v", err)
			}
		}
	}()

	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"status": "running"})
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
		WALSize      int64  `json:"wal_size"`
		SHMSize      int64  `json:"shm_size"`
		TotalOnDisk  int64  `json:"total_on_disk"`
		RSSBytes     int64  `json:"rss_bytes"`

		GoAllocBytes        uint64 `json:"go_alloc_bytes"`
		GoHeapInuseBytes    uint64 `json:"go_heap_inuse_bytes"`
		GoHeapIdleBytes     uint64 `json:"go_heap_idle_bytes"`
		GoHeapReleasedBytes uint64 `json:"go_heap_released_bytes"`
		GoSysBytes          uint64 `json:"go_sys_bytes"`
		GoNumGC             uint32 `json:"go_num_gc"`
		CgroupCurrent       int64  `json:"cgroup_memory_current_bytes"`
		CgroupAnon          int64  `json:"cgroup_memory_anon_bytes"`
		CgroupFile          int64  `json:"cgroup_memory_file_bytes"`
		Warning             string `json:"warning,omitempty"`
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
		resp.WALSize = stats.WALSize
		resp.SHMSize = stats.SHMSize
		resp.TotalOnDisk = stats.TotalOnDisk
	}

	// 3) Memory stats (best-effort; should never fail /status)
	{
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		resp.GoAllocBytes = ms.Alloc
		resp.GoHeapInuseBytes = ms.HeapInuse
		resp.GoHeapIdleBytes = ms.HeapIdle
		resp.GoHeapReleasedBytes = ms.HeapReleased
		resp.GoSysBytes = ms.Sys
		resp.GoNumGC = ms.NumGC
	}

	if rss, err := procSelfRSSBytes(); err != nil {
		addWarning(fmt.Sprintf("rss unavailable: %v", err))
	} else {
		resp.RSSBytes = rss
	}

	if cg, err := cgroupV2Memory(); err != nil {
		// Common on non-systemd or cgroup v1; keep quiet unless daemon is idle (helps debugging).
		if !running {
			addWarning(fmt.Sprintf("cgroup mem unavailable: %v", err))
		}
	} else {
		resp.CgroupCurrent = cg.Current
		resp.CgroupAnon = cg.Anon
		resp.CgroupFile = cg.File
	}

	writeJSON(w, resp)
}

func procSelfRSSBytes() (int64, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		// Format: VmRSS:\t  12345 kB
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("unexpected VmRSS format: %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("VmRSS not found")
}

type cgroupMemInfo struct {
	Current int64
	Anon    int64
	File    int64
}

func cgroupV2Memory() (cgroupMemInfo, error) {
	// cgroup v2 path is in /proc/self/cgroup as: 0::/some/path
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return cgroupMemInfo{}, err
	}

	var rel string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if err := sc.Err(); err != nil {
		return cgroupMemInfo{}, err
	}
	if rel == "" {
		return cgroupMemInfo{}, fmt.Errorf("cgroup v2 path not found")
	}

	base := "/sys/fs/cgroup" + rel
	current, err := readInt64File(base + "/memory.current")
	if err != nil {
		return cgroupMemInfo{}, err
	}

	stat, err := os.ReadFile(base + "/memory.stat")
	if err != nil {
		return cgroupMemInfo{}, err
	}

	var anon, file int64
	sc = bufio.NewScanner(bytes.NewReader(stat))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "anon":
			anon = v
		case "file":
			file = v
		}
	}
	if err := sc.Err(); err != nil {
		return cgroupMemInfo{}, err
	}

	return cgroupMemInfo{Current: current, Anon: anon, File: file}, nil
}

func readInt64File(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	return strconv.ParseInt(s, 10, 64)
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
		if errors.Is(err, storage.ErrDirectoryNotFound) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]interface{}{
		"path": path,
		"size": total,
	})
}

func (d *daemon) handleSubfolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := queryPathOrRoot(r.URL.Query().Get("path"))

	results, err := d.store.GetDirectSubfolders(ctx, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, results)
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
		entryType = "file" // default to file if not specified
	}

	entry := indexing.IndexEntry{
		RelativePath: relPath,
		AbsolutePath: absPath,
		Name:         payload.Name,
		Size:         payload.Size,
		ModTime:      time.Unix(modUnix, 0),
		Type:         entryType,
		Hidden:       payload.Hidden,
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
  "info": { "title": "Indexer API", "version": "2.0.0" },
  "paths": {
    "/index": { "post": { "summary": "Trigger full index", "responses": { "202": {"description": "Started"}, "409": {"description": "Already running"} } } },
    "/reindex": { "post": { "summary": "Reindex a specific path", "parameters": [{ "in": "query", "name": "path", "required": true, "schema": {"type": "string"}, "description": "Path to reindex (e.g., /home/user)" }], "responses": { "202": {"description": "Started"}, "400": {"description": "Path required"}, "409": {"description": "Already running"} } } },
    "/vacuum": { "post": { "summary": "Reclaim disk space (VACUUM)", "responses": { "202": {"description": "Started"}, "409": {"description": "Already running"} } } },
    "/status": { "get": { "summary": "Get status", "responses": { "200": {"description": "Status"} } } },
    "/search": { "get": { "summary": "Search entries (returns type: folder/file)", "parameters": [{ "in": "query", "name": "q", "schema": {"type": "string"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Results with type field indicating folder or file"} } } },
    "/subfolders": { "get": { "summary": "Get direct subfolders with sizes", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"}, "description": "Parent path (defaults to /)" }], "responses": { "200": {"description": "Array of direct subfolders with their sizes"} } } },
    "/dirsize": { "get": { "summary": "Directory size", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "Size"} } } },
    "/entries": { "get": { "summary": "List entries (returns type: folder/file)", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }, { "in": "query", "name": "recursive", "schema": {"type": "boolean"} }, { "in": "query", "name": "limit", "schema": {"type": "integer"} }, { "in": "query", "name": "offset", "schema": {"type": "integer"} }], "responses": { "200": {"description": "Entries with type field indicating folder or file"} } } },
    "/add": { "post": { "summary": "Upsert entry", "responses": { "200": {"description": "OK"} } } },
    "/delete": { "delete": { "summary": "Delete entry", "parameters": [{ "in": "query", "name": "path", "schema": {"type": "string"} }], "responses": { "200": {"description": "OK"} } } }
  }
}`
