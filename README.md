[![Go Report Card](https://goreportcard.com/badge/github.com/mordilloSan/indexer)](https://goreportcard.com/report/github.com/mordilloSan/indexer)

# Indexer

This software was built as proof of concept for a filesystem indexer using a SQLite database, written in Go.

The goal was to have a permanently running daemon with minimal memory footprint, fast(ish) full indexing, and efficient query responses.

## Contents

- [Features](#features)
- [Quickstart](#quickstart)
- [Installation](#installation)
  - [Install from local build (Go required)](#install-from-local-build-go-required)
  - [Install from GitHub Releases (no Go required)](#install-from-github-releases-no-go-required)
- [CLI flags](#cli-flags)
- [API](#api)
- [Architecture](#architecture)
- [Database schema](#database-schema)
- [Development](#development)
- [Configuration file (`/etc/default/indexer`)](#configuration-file-etcdefaultindexer)
- [Performance](#performance)
- [Limitations](#limitations)
- [License](#license)

## Features

- Daemonized HTTP API on a Unix socket by default; optional TCP listener for remote access.
- Streaming writes to SQLite (500-entry batches) to keep memory low (~150 MB for ~1M files).
- Server-Sent Events (SSE) streaming for real-time progress updates during reindex and vacuum operations.
- Hardlink-aware size accounting so totals match `du`; deleted entries are cleaned after each run.
- Auto-index on a fixed interval plus manual `/index` endpoint; hidden file support is opt-in.
- WAL-enabled SQLite schema with incremental auto-vacuum and index pruning for automatic space reclamation.
- Small store layer for search, dirsize, and path queries.

## Quickstart

```bash
make run
```

That should produce the following logs:

```bash
API listening on unix:///tmp/indexer.sock
API listening on http://localhost:9999
```

#### Health and basic queries
```bash
curl --unix-socket /tmp/indexer.sock http://localhost/status
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/index
curl --unix-socket /tmp/indexer.sock 'http://localhost/search?q=log&limit=20'
```

If you installed via systemd, you can also query status with:

```bash
indexer --status
```

## Installation

### Install from local build (Go required)

An installation script is provided at `scripts/local_install.sh` that automates the setup (builds from source):

```bash
sudo ./scripts/local_install.sh
```

The script performs the following steps:
1. Builds the binary and installs it to `/usr/local/bin/indexer`
2. Installs systemd service and socket units from the `systemd/` directory
3. Creates `/etc/default/indexer` configuration file with environment variables
4. Enables socket activation and starts the daemon
5. Verifies the installation

### Install from GitHub Releases (no Go required)

If you prefer not to build locally, use the release installer (downloads the prebuilt binary and systemd unit files from GitHub Releases):

```bash
curl -fsSL https://github.com/mordilloSan/indexer/releases/latest/download/indexer-install.sh | sudo bash
```

To install a specific version:

```bash
curl -fsSL https://github.com/mordilloSan/indexer/releases/download/v1.2.0/indexer-install.sh | sudo bash
```

After installation, edit `/etc/default/indexer` to configure the path to index, interval, and other options. Systemd socket activation is used by default, so the daemon starts on-demand when the socket is accessed.

Notes:
- The installers place the binary at `/usr/local/bin/indexer` (usually already on `$PATH`).
- When installed via systemd, you typically manage the daemon via `systemctl`, but you can still run `indexer --version` / `indexer --help` from your shell.
- Avoid running a second daemon manually against the same `--socket-path` / `--db-path` while the systemd service is running.

Quick checks:

```bash
command -v indexer || echo "indexer not on PATH (try /usr/local/bin/indexer)"
indexer --version
sudo systemctl status indexer.service --no-pager
```

## CLI flags

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
| `--status` | `false` | Query `/status` from a running daemon and exit |
| `--version` | `false` | Print version/build info and exit |

On startup (CLI or systemd), `indexer` logs its version/build info.

## API

HTTP is served on the Unix socket (default) or on the TCP listener if `--listen` is set. The same JSON schema is returned in both cases.

#### `POST /index`

Trigger a full index in the background.

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/index
```

Response: `{"status":"running"}` with `202 Accepted`. Returns `409 Conflict` if an index is already running.

#### `POST /reindex?path=<subpath>`

Re-scan a specific subdirectory within the indexed root. The `path` query parameter is required and must be an absolute path relative to the index (e.g., `/home/alice/docs`). During the run, existing entries under that subpath are deleted, the filesystem is re-walked, and ancestor directory sizes are updated.

```bash
curl --unix-socket /tmp/indexer.sock -X POST 'http://localhost/reindex?path=/projects/work'
```

Response: `{"status":"running","path":"/projects/work"}` with `202 Accepted`. Returns `400` when `path` is missing, or `409` if another index/reindex is already running.

#### `POST /vacuum`

Reclaim disk space by running SQLite `VACUUM` in the background. This can take a while on large databases and requires an exclusive lock, so queries may block briefly while it runs.

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/vacuum
```

Response: `{"status":"running"}` with `202 Accepted`. Returns `409 Conflict` if another index/reindex/vacuum is already running.

#### `POST /prune?keep_latest=<n>&max_age_days=<days>`

Prune old index records and their entries to reclaim database space. This removes index records that haven't been updated recently, keeping only the most recent ones. The associated entries are automatically deleted due to foreign key constraints.

```bash
# Use defaults (keep 1 latest index, delete indexes older than 30 days)
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/prune

# Keep 3 most recent indexes, delete anything older than 7 days
curl --unix-socket /tmp/indexer.sock -X POST 'http://localhost/prune?keep_latest=3&max_age_days=7'
```

**Query parameters:**
- `keep_latest` (default: `1`) - Number of most recent indexes to keep (minimum 1)
- `max_age_days` (default: `30`) - Maximum age in days for indexes to keep

Response: `{"status":"running","keep_latest":"1","max_age_days":"30"}` with `202 Accepted`.

Returns `409 Conflict` if another index/reindex/vacuum/prune is already running.

**Note:** Pruning is useful when you've re-indexed multiple times and want to clean up old data. After pruning, consider running `/vacuum` to fully compact the database file. The database uses incremental auto-vacuum, so space is reclaimed automatically, but a full `VACUUM` defragments the file.

#### `GET /status?stream=true` (SSE attach while work is running)

Use a single SSE endpoint for all active background work (`index`, `reindex`, `vacuum`, `prune`).

```bash
curl --unix-socket /tmp/indexer.sock -N -H 'Accept: text/event-stream' 'http://localhost/status?stream=true'
```

If no work is running, `/status` falls back to normal JSON status.

Event names:
- `started`: includes operation metadata (for example `{"status":"running","operation":"reindex","path":"/projects/work"}`)
- `progress`: operation-specific progress (for example files/dirs for reindex, phase/message for vacuum/prune)
- `complete`: final operation stats
- `error`: `{"message":"..."}`

#### `GET /status`

Returns daemon state and stats from the most recent index.

```bash
curl --unix-socket /tmp/indexer.sock http://localhost/status
```

Example:
```json
{"status":"idle","num_dirs":0,"num_files":0,"total_size":0,"last_indexed":"2025-01-15T10:30:45Z","total_indexes":1,"total_entries":0,"database_size":0,"wal_size":0,"shm_size":0,"total_on_disk":0}
```

Notes:
- `last_indexed` may be empty if the index has never been run.
- When the daemon is indexing and the DB is temporarily unavailable, the response may include a `warning` field.
- `database_size` is the main SQLite file only; `total_on_disk` includes `-wal` and `-shm` sidecars.
- When work is running, `active_operation` and `active_path` may be present in the JSON response.

#### `GET /search?q=<term>&limit=<n>`

Substring name search using SQLite `LIKE`. Returns both files and folders with a `type` field for clear identification.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/search?q=nginx&limit=50'
```

Response (fields are relative to the indexed root):
```json
[{"path":"/etc/nginx/nginx.conf","name":"nginx.conf","type":"file","size":2048,"mod_time":"2025-01-15T08:22:11Z","inode":1234567}]
```

The `type` field is either `"file"` or `"folder"` for easy filtering.

If there is no index yet, the endpoint returns an empty list.

#### `GET /entries?path=<path>&recursive=<bool>&limit=<n>&offset=<n>`

List entries at or under a path. `recursive=true` returns the full subtree; without it you get the entry that matches `path`. Each entry includes a `type` field (`"file"` or `"folder"`) for clear identification.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/entries?path=/home&recursive=true&limit=100&offset=200'
```

If there is no index yet, the endpoint returns an empty list.

#### `GET /subfolders?path=<path>`

Get direct child folders of a path with their pre-calculated sizes (non-recursive). This is useful for building directory browsers or disk usage visualizations.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/subfolders?path=/home'
```

Response:
```json
[
  {"path":"/home/alice","name":"alice","size":104857600,"mod_time":"2025-01-15T10:30:45Z"},
  {"path":"/home/bob","name":"bob","size":209715200,"mod_time":"2025-01-15T11:22:33Z"}
]
```

The `path` parameter defaults to `/` (root) if omitted. Only directories are returned, sorted alphabetically by name.

If there is no index yet, the endpoint returns an empty list.

#### `GET /dirsize?path=<path>`

Aggregate size for a path (inclusive).

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/dirsize?path=/var'
```

Response:
```json
{"path":"/var","size":5368709120}
```

Returns `400` if the directory is not present in the latest index.

#### `POST /add`

Manually upsert a single entry into the latest index (useful for incremental updates).

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/add \
  -H 'Content-Type: application/json' \
  -d '{"path":"/data/newfile.txt","absPath":"/mnt/storage/data/newfile.txt","name":"newfile.txt","size":1024,"type":"file","hidden":false,"modUnix":1705315845,"inode":9876543}'
```

Response: `{"status":"ok"}`.

Returns `400` if no index exists yet.

Both `/add` and `/delete` update ancestor directory sizes so folder totals stay correct between full indexing runs.

#### `DELETE /delete?path=<path>`

Delete a single entry from the latest index and propagate the size change up the directory tree.

```bash
curl --unix-socket /tmp/indexer.sock -X DELETE 'http://localhost/delete?path=/data/newfile.txt'
```

Response: `{"status":"ok"}` (no-op if the path was not present).

Returns `400` if no index exists yet.

#### `GET /openapi.json`

OpenAPI 3.0 document for the API (version 2.2.0).

## Architecture

```
indexer/
├── main.go              # Entry point, flag parsing, daemon setup
├── cmd/
│   ├── daemon.go        # HTTP server (Unix socket + TCP) setup
│   ├── handlers.go      # API request handlers
│   └── handlers_sse.go  # Server-Sent Events streaming handlers
├── storage/
│   ├── db.go            # SQLite schema and core database operations
│   └── queries.go       # Query API (search, dirsize, entries, subfolders)
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
| `name` | TEXT | File/directory name |
| `size` | INTEGER | Logical size in bytes |
| `mod_time` | INTEGER | Unix timestamp of last modification |
| `type` | TEXT | `"file"` or `"directory"` |
| `hidden` | INTEGER | 1 if hidden, 0 otherwise |
| `inode` | INTEGER | Inode number (hardlink detection) |
| `last_seen` | INTEGER | Timestamp of most recent scan |

Indexes: `idx_entries_index_id` on `index_id`, and `idx_entries_path` unique on `(index_id, relative_path)`.

## Development

- Build: `make build` (or `go build -o indexer .`)
- Tests: `make test`
- Lint: `make golint`
- Benchmarks: `make benchmark`

### Benchmarking

The helper script in `scripts/benchmark.sh` spins up `indexer` in `--index-mode`, captures wall-clock time, RSS samples, and database stats, then appends a Markdown table to `benchmark.md`. Run it directly:

```bash
make benchmark
```


You can tweak the workload by exporting environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `TEST_PATH` | Filesystem root to index (must exist). | `/` |
| `DATASET_DIR` | Directory populated with synthetic files; auto-created under `/tmp` when empty. | random temp dir |
| `DATASET_FILES` | Number of synthetic files to create when `DATASET_DIR` is managed by the script. | `1000` |
| `INCLUDE_HIDDEN` | When `true`, passes `--include-hidden` to the benchmarked run (matching the CLI flag). | `true` |
| `LISTEN_ADDR` | Optional TCP listener passed to the daemon; leave empty to benchmark over the Unix socket. | unset |
| `OUTPUT_FILE` | Markdown file that collects benchmark summaries. | `benchmark.md` |

All other knobs (e.g., `DB_PATH`, `SOCKET_PATH`, `TOP_SNAPSHOT`) can also be overridden—check the script header for defaults. The script cleans up its temporary dataset and reports any failures with links to the captured logs.

## Configuration file (`/etc/default/indexer`)

When you install via `scripts/local_install.sh`, `scripts/global_install.sh`, or the GitHub Release installer, the systemd unit reads `/etc/default/indexer` (if present) and uses it to set the daemon flags.

If `/etc/default/indexer` is not present, the systemd unit still starts using its built-in defaults (it uses `EnvironmentFile=-/etc/default/indexer`, where the leading `-` means “optional”).

Common settings:

```bash
# Filesystem root to index
INDEXER_PATH=/data

# Index name (identifier)
INDEXER_NAME=data

# Include dotfiles/dotdirs (true/false)
INDEXER_INCLUDE_HIDDEN=false

# SQLite DB path
INDEXER_DB_PATH=/tmp/indexer.db

# Auto-index interval (e.g., 6h, 30m); set 0 to disable
INDEXER_INTERVAL=6h

# Optional extra flags appended to the daemon command line
# (Example: expose HTTP over TCP as well as the Unix socket)
INDEXER_LISTEN_FLAG=--listen=:8080
```

Apply changes:

```bash
sudo systemctl restart indexer.service
```

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

## Database Maintenance

The database uses SQLite's incremental auto-vacuum feature to automatically reclaim space when records are deleted. However, you may want to manually manage database growth:

### Pruning Old Indexes

If you re-index frequently, old index records can accumulate. Use the `/prune` endpoint to clean them up:

```bash
# Keep only the latest index, remove anything older than 30 days
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/prune

# Keep 5 most recent indexes, remove anything older than 7 days
curl --unix-socket /tmp/indexer.sock -X POST 'http://localhost/prune?keep_latest=5&max_age_days=7'
```

### Full Database Compaction

After pruning or when the database has accumulated fragmentation, run a full `VACUUM`:

```bash
curl --unix-socket /tmp/indexer.sock -X POST http://localhost/vacuum
```

### Monitoring Database Size

Check the current database size via the `/status` endpoint:

```bash
curl --unix-socket /tmp/indexer.sock http://localhost/status | jq '{database_size, wal_size, shm_size, total_on_disk}'
```

## License

See `LICENSE`. Inspired by [Filebrowser Quantum](https://github.com/gtsteffaniak/filebrowser).
