[![Go Report Card](https://goreportcard.com/badge/github.com/mordilloSan/indexer)](https://goreportcard.com/report/github.com/mordilloSan/indexer)

# Indexer

Streaming filesystem indexer daemon that snapshots directory trees into SQLite and exposes search/dirsize APIs over Unix sockets or TCP. Built for quick queries without re-walking the filesystem.

## Highlights

- Daemonized HTTP API on a Unix socket by default; optional TCP listener for remote access.
- Streaming writes to SQLite (500-entry batches) to keep memory low (~150 MB for ~1M files).
- Hardlink-aware size accounting so totals match `du`; deleted entries are cleaned after each run.
- Auto-index on a fixed interval plus manual `/index` endpoint; hidden file support is opt-in.
- WAL-enabled SQLite schema with a small store layer for search, dirsize, and path queries.

## Quickstart

```bash
go build -o indexer .
./indexer --path /data --socket-path /tmp/indexer.sock --db-path /tmp/indexer.db

# Health and basic queries
curl --unix-socket /tmp/indexer.sock http://localhost/status
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/index
curl --unix-socket /tmp/indexer.sock 'http://localhost/search?q=log&limit=20'
```

Add `--listen :8080` to expose the API over TCP instead of (or alongside) the Unix socket.

### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--path` | **(required)** | Filesystem root to index |
| `--name` | sanitized `path` | Index name (alphanumeric identifier) |
| `--include-hidden` | `false` | Include dotfiles and dotdirs |
| `--db-path` | `/tmp/indexer.db` | SQLite database path (or `$INDEXER_DB_PATH`) |
| `--socket-path` | `/var/run/indexer.sock` | Unix socket path for API |
| `--listen` | *(disabled)* | TCP address for HTTP API (e.g., `:8080`) |
| `--interval` | `0` (off) | Auto-index interval (`6h`, `30m`, etc.) |
| `--verbose` | `false` | Enable debug logging |

## API

HTTP is served on the Unix socket (default) or on the TCP listener if `--listen` is set. The same JSON schema is returned in both cases.

### `POST /index`

Trigger a full index in the background.

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/index
```

Response: `{"status":"running"}` with `202 Accepted`. Returns `409 Conflict` if an index is already running.

### `GET /status`

Returns daemon state and stats from the most recent index.

```bash
curl --unix-socket /tmp/indexer.sock http://localhost/status
```

Example:
```json
{"status":"idle","num_dirs":0,"num_files":0,"total_size":0,"last_indexed":"2025-01-15T10:30:45Z"}
```

### `GET /search?q=<term>&limit=<n>`

Substring name search using SQLite `LIKE`.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/search?q=nginx&limit=50'
```

Response (fields are relative to the indexed root):
```json
[{"path":"/etc/nginx/nginx.conf","name":"nginx.conf","is_dir":false,"size":2048,"mod_time":"2025-01-15T08:22:11Z","inode":1234567}]
```

### `GET /entries?path=<path>&recursive=<bool>&limit=<n>&offset=<n>`

List entries at or under a path. `recursive=true` returns the full subtree; without it you get the entry that matches `path`.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/entries?path=/home&recursive=true&limit=100&offset=200'
```

### `GET /dirsize?path=<path>`

Aggregate size for a path (inclusive).

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/dirsize?path=/var'
```

Response:
```json
{"path":"/var","size":5368709120}
```

### `POST /add`

Manually upsert a single entry into the latest index (useful for incremental updates).

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/add \
  -H 'Content-Type: application/json' \
  -d '{"path":"/data/newfile.txt","absPath":"/mnt/storage/data/newfile.txt","name":"newfile.txt","size":1024,"isDir":false,"type":"file","hidden":false,"modUnix":1705315845,"inode":9876543}'
```

Response: `{"status":"ok"}`.

Both `/add` and `/delete` update ancestor directory sizes so folder totals stay correct between full indexing runs.

### `DELETE /delete?path=<path>`

Delete a single entry from the latest index and propagate the size change up the directory tree.

```bash
curl --unix-socket /tmp/indexer.sock -X DELETE 'http://localhost/delete?path=/data/newfile.txt'
```

Response: `{"status":"ok"}` (no-op if the path was not present).

### `GET /openapi.json`

Minimal OpenAPI 3.0 document for the API.

## Architecture

```
indexer/
├── main.go              # Entry point, flag parsing, daemon setup
├── cmd/daemon.go        # HTTP server (Unix socket + TCP), API handlers
├── storage/
│   ├── storage.go       # SQLite schema, streaming writer, CRUD operations
│   └── store.go         # Query API (search, dirsize, entries, upsert)
└── indexing/
    ├── indexingFiles.go # Filesystem traversal and aggregation
    ├── export.go        # Index-to-entries serialization
    ├── unix.go          # Unix-specific syscalls (inode, hardlinks)
    └── iteminfo/        # Data structures and helpers
```

## Database schema

SQLite with WAL and foreign keys on. Key tables:

### `indexes`

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER (PK) | Auto-increment ID |
| `name` | TEXT (UNIQUE) | Index name |
| `root_path` | TEXT | Filesystem root being indexed |
| `source` | TEXT | Source identifier (often same as `root_path`) |
| `include_hidden` | INTEGER | 1 if hidden files included, 0 otherwise |
| `num_dirs` | INTEGER | Total directory count |
| `num_files` | INTEGER | Total file count |
| `total_size` | INTEGER | Total bytes (hardlink-aware) |
| `disk_used` | INTEGER | Root directory size |
| `disk_total` | INTEGER | Filesystem capacity (if available) |
| `last_indexed` | INTEGER | Unix timestamp of last index |
| `index_duration_ms` | INTEGER | Indexing duration (ms) |
| `export_duration_ms` | INTEGER | Export duration (ms) |
| `vacuum_duration_ms` | INTEGER | Vacuum duration (ms) |
| `created_at` | INTEGER | Unix timestamp of creation |

### `entries`

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER (PK) | Auto-increment ID |
| `index_id` | INTEGER (FK) | References `indexes.id` |
| `relative_path` | TEXT | Path relative to index root |
| `absolute_path` | TEXT | Full filesystem path |
| `name` | TEXT | File/directory name |
| `size` | INTEGER | Logical size in bytes |
| `mod_time` | INTEGER | Unix timestamp of last modification |
| `type` | TEXT | `"file"` or `"directory"` |
| `hidden` | INTEGER | 1 if hidden, 0 otherwise |
| `is_dir` | INTEGER | 1 if directory, 0 if file |
| `inode` | INTEGER | Inode number (hardlink detection) |
| `last_seen` | INTEGER | Timestamp of most recent scan |

Indexes: `idx_entries_index_id` on `index_id`, and `idx_entries_path` unique on `(index_id, relative_path)`.

## Development

- Build: `make build` (or `go build -o indexer .`)
- Tests: `go test ./...`
- Benchmarks: `./scripts/benchmark.sh` writes results to `benchmark.md` and captures memory stats.

## Systemd

An installation script is provided at `scripts/global_install.sh` that automates the setup:

```bash
sudo ./scripts/global_install.sh
```

The script performs the following steps:
1. Builds the binary and installs it to `/usr/local/bin/indexer`
2. Installs systemd service and socket units from the `systemd/` directory
3. Creates `/etc/default/indexer` configuration file with environment variables
4. Enables socket activation and starts the daemon
5. Verifies the installation

After installation, edit `/etc/default/indexer` to configure the path to index, interval, and other options. Systemd socket activation is used by default, so the daemon starts on-demand when the socket is accessed.

Service files are available in the `systemd/` directory for reference.

## Performance

- Indexing speed: ~50k files/sec on SSD (warm cache)
- Memory usage: ~150 MB for ~1M files in streaming mode
- DB size: ~500 bytes per entry (WAL enabled)
- Search latency: typically <10 ms for substring queries

## Limitations

- Runs with invoking user permissions (use `root` for full system coverage).
- Single writer; multiple readers supported via WAL.
- Filesystem changes between scans are not watched automatically; run an index or use `/add`/`/delete` to keep the index aligned with file operations.
- Symlinks are resolved to targets; `/proc`, `/dev`, and network mounts are skipped on Linux.

## License

See `LICENSE`. Inspired by [Filebrowser Quantum](https://github.com/gtsteffaniak/filebrowser).
