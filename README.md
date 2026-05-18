[![Go Report Card](https://goreportcard.com/badge/github.com/mordilloSan/indexer)](https://goreportcard.com/report/github.com/mordilloSan/indexer)

# Indexer

This software was built as proof of concept for a filesystem indexer using a SQLite database, written in Go.

The goal is to provide a low-memory filesystem index with an on-demand API, fast(ish) full indexing, and efficient query responses.

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
- [Configuration file](#configuration-file)
- [Performance](#performance)
- [Limitations](#limitations)
- [License](#license)

## Features

- Socket-activated HTTP API on a Unix socket by default; optional TCP listener for remote access.
- Full-screen terminal dashboard for stats, service state, logs, and common actions.
- Streaming writes to SQLite (500-entry batches) to keep memory low (~150 MB for ~1M files).
- Server-Sent Events (SSE) streaming for real-time progress updates during reindex and vacuum operations.
- Hardlink-aware size accounting so totals match `du`; deleted entries are cleaned after each run.
- Systemd timer based auto-indexing plus manual `/index` endpoint; hidden files and network mounts are opt-in.
- WAL-enabled SQLite schema with incremental auto-vacuum and index pruning for automatic space reclamation.
- Small store layer for search, dirsize, entry counts, and path queries.

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
sudo indexer status
```

To index a folder once and exit:

```bash
indexer index --path /data --db-path /tmp/indexer.db
```

Network mounts such as NFS, SMB, and CIFS are skipped by default during recursive traversal. Enable them explicitly when you want to index a mounted share:

```bash
indexer index --path /mnt/share --include-network-mounts --db-path /tmp/indexer.db
```

## Installation

### Install from local build (Go required)

An installation script is provided at `scripts/local_install.sh` that automates the setup (builds from source):

```bash
sudo ./scripts/local_install.sh
```

The script performs the following steps:
1. Builds the binary and installs it to `/usr/local/bin/indexer`
2. Installs systemd service, socket, and timer units from the `systemd/` directory
3. Creates `/etc/indexer/config.json` configuration file
4. Enables the API socket and index timer
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

After installation, edit `/etc/indexer/config.json` to configure the path to index, interval, and other options. Systemd socket activation is used by default, so the API daemon starts on demand when the socket is accessed and exits after being idle. Scheduled indexing runs as `indexer-index.service` from `indexer-index.timer`, and `indexer.target` groups the socket and timer as the single systemd control point.

Notes:
- The installers place the binary at `/usr/local/bin/indexer` (usually already on `$PATH`).
- When installed via systemd, `indexer.socket` is the always-on API entrypoint and `indexer-index.timer` owns scheduled indexing. `systemctl start|stop indexer.target` controls both without eagerly starting the API daemon.
- Avoid running a second daemon manually against the same `--socket-path` / `--db-path` while the systemd socket/service is active.

Quick checks:

```bash
command -v indexer || echo "indexer not on PATH (try /usr/local/bin/indexer)"
indexer version
sudo systemctl status indexer.target --no-pager
sudo systemctl status indexer.socket --no-pager
sudo systemctl status indexer-index.timer --no-pager
```

## CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config-file` | `/config/indexer.json` when `/config` exists, otherwise `/etc/indexer/config.json` | JSON config file |
| `--path` | `/` | Filesystem root to index |
| `--name` | `root` | Index name (alphanumeric identifier) |
| `--include-hidden` | `true` | Include dotfiles and dotdirs |
| `--include-network-mounts` | `false` | Traverse network/external mounts such as NFS, SMB, and CIFS |
| `--fresh` | `true` | Clear existing index data before a full index |
| `--keep-indexes` | `0` | Automatically prune old index records after indexing; `0` disables automatic pruning. Use with `--fresh=false` when keeping history. |
| `--db-path` | `/tmp/indexer.db` | SQLite database path |
| `--db-busy-timeout` | `5s` | SQLite busy timeout |
| `--db-journal-mode` | `WAL` | SQLite journal mode |
| `--db-synchronous` | `OFF` | SQLite synchronous setting |
| `--db-auto-vacuum` | `INCREMENTAL` | SQLite auto-vacuum setting |
| `--db-max-open-conns` | `5` | SQLite max open connections |
| `--db-max-idle-conns` | `2` | SQLite max idle connections |
| `--db-conn-max-idle-time` | `5m0s` | SQLite idle connection lifetime |
| `--socket-path` | `/var/run/indexer.sock` | Unix socket path for API |
| `--listen` | *(disabled)* | TCP address for HTTP API (e.g., `:8080`) |
| `--interval` | `1h0m0s` | Auto-index interval (`6h`, `30m`, etc.); `0` disables |
| `--idle-timeout` | `0` | Daemon-only idle exit timeout; packaged systemd service uses `2m` |
| `--verbose` | `false` | Enable debug logging |

Daemon config load order is: built-in defaults, JSON config file, `INDEXER_*` environment overrides, then command-line/systemd flags.

Commands:
- `indexer daemon ...` starts the API daemon. The built-in scheduler still works when `--interval` is non-zero, but native systemd units disable it and use `indexer-index.timer`.
- `indexer index ...` indexes one folder and exits. It accepts `--config-file`, `--path`, `--name`, `--include-hidden`, `--include-network-mounts`, `--fresh`, `--keep-indexes`, database flags, `--cpu-profile`, and `--verbose`.
- `indexer status` queries the API daemon and may socket-activate it.
- `indexer config` reads the JSON config file by default; add `--runtime` to query the daemon.
- `indexer config apply` syncs the current JSON config to systemd target/socket/timer drop-ins.
- `indexer service status|logs|run-now` wraps common systemd status, journal, and one-shot index operations.
- `indexer dashboard` opens a full-screen terminal dashboard for stats, unit state, logs, and confirmed actions.
- `indexer setup` opens a plain terminal wizard for editing the JSON config file and applying systemd target/timer/socket changes.
- `indexer version` prints version/build info.

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

#### `GET /entrycount?path=<path>`

Count files and directories at and under a path. The path itself is included when it exists in the latest index, so counting a directory includes that directory entry in `dirs`; counting an indexed file returns `files: 1, dirs: 0`.

```bash
curl --unix-socket /tmp/indexer.sock 'http://localhost/entrycount?path=/home'
```

Response:
```json
{"path":"/home","files":42891,"dirs":1320}
```

The `path` parameter defaults to `/` if omitted. For `path=/`, the root `/` directory entry is included, so `dirs` is one greater than the `/status` `num_dirs` value, which tracks child directories only. If there is no index yet, or the path is not present in the latest index, the endpoint returns zero counts.

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

#### `GET /config`

Return the persisted admin configuration JSON.

```bash
curl --unix-socket /tmp/indexer.sock http://localhost/config
```

Example:
```json
{
  "index_path": "/",
  "index_name": "root",
  "include_hidden": true,
  "include_network_mounts": false,
  "fresh_index": true,
  "keep_indexes": 0,
  "db_path": "/tmp/indexer.db",
  "db_busy_timeout": "5s",
  "db_journal_mode": "WAL",
  "db_synchronous": "OFF",
  "db_auto_vacuum": "INCREMENTAL",
  "db_max_open_conns": 5,
  "db_max_idle_conns": 2,
  "db_conn_max_idle_time": "5m0s",
  "socket_path": "/var/run/indexer.sock",
  "listen_addr": "",
  "interval": "1h0m0s"
}
```

#### `PUT /config`

Update the persisted admin configuration. Writes are accepted only over the Unix socket; TCP requests return `403`.

```bash
curl --unix-socket /tmp/indexer.sock -X PUT http://localhost/config \
  -H 'Content-Type: application/json' \
  -d '{"index_path":"/data","interval":"6h"}'
```

The daemon validates the JSON, writes the config file atomically, and applies runtime-safe fields immediately. Changes to `db_path`, any `db_*` SQLite setting, `socket_path`, or `listen_addr` are persisted but require the disposable API daemon to restart to fully take effect; those responses include `X-Indexer-Restart-Required: true`.

#### `GET /openapi.json`

OpenAPI 3.0 document for the API (version 2.3.0).

## Architecture

```
indexer/
├── main.go              # Thin CLI entrypoint
├── internal/
│   ├── configfile/
│   │   └── config.go        # Stable JSON config model, validation, atomic writes
│   └── cli/
│       ├── main.go          # Command dispatch and daemon startup
│       ├── config.go        # Non-interactive config file updates
│       ├── dashboard.go     # Full-screen terminal dashboard
│       ├── systemd.go       # Systemd status/log/action helpers
│       ├── setup.go         # Interactive setup wizard
│       ├── envfile.go       # Legacy systemd EnvironmentFile parser helpers
│       ├── daemon_client.go # CLI client for daemon status/config endpoints
│       ├── index_command.go # One-shot index command and index-mode subprocess
│       └── util.go          # Shared CLI helpers
├── cmd/
│   ├── daemon.go        # HTTP server (Unix socket + TCP) setup
│   ├── handlers.go      # API request handlers
│   ├── operation_lock.go # Cross-process guard for indexing/maintenance writes
│   └── handlers_sse.go  # Server-Sent Events streaming handlers
├── storage/
│   ├── db.go            # SQLite schema and core database operations
│   ├── queries.go       # Query API (search, dirsize, entrycount, entries, subfolders)
│   └── maintenance.go   # Vacuum and pruning helpers
└── indexing/
    ├── traversal.go     # Filesystem traversal
    ├── index.go         # Index aggregation and persistence
    ├── files.go         # File metadata helpers
    ├── mounts.go        # Mount classification
    └── types.go         # Shared indexing types
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

- Build: `make build`
- Compile only: `make build-only` (or `go build -o indexer .`)
- Local install: `make localinstall`
- Tests: `make test`
- Lint: `make golint`
- Benchmarks: `make benchmark`
- Generate PGO profile: `make pgo`
- Compare PGO vs no-PGO: `make compare-pgo`

### PGO Workflow

`make pgo` builds a non-PGO binary, profiles the real filesystem workload at `/` by default, captures representative CPU profiles for a fresh and incremental index run, merges them into `default.pgo`, and prints a short `pprof` summary.

`default.pgo` is kept as optional profiling data for experiments. The default local and CI builds no longer use `-pgo=auto`.

In the default `real` mode, `make pgo` authenticates with `sudo` up front and runs the profiling passes as root so indexing `/` does not get blocked by permission errors. The build and merged `default.pgo` output remain owned by your user.

You can override the default workload path:

```bash
PGO_MODE=real PGO_PATH=/some/path make pgo
```

Useful knobs:

- `PGO_MODE=real|synthetic` chooses between an existing filesystem path and a generated dataset. Default: `real`.
- `PGO_PATH=/path` selects the real workload path. Default: `/`.
- `PGO_INCLUDE_HIDDEN=true|false` controls whether hidden files are included while profiling.
- `PGO_USE_SUDO=true|false` controls whether the profiling runs use `sudo`. Default: `true` for real mode, `false` for synthetic mode.
- `OUTPUT_PGO=/path/to/default.pgo` writes the merged profile somewhere other than the repo root.

In `real` mode the script does not mutate the target tree; it profiles one fresh pass and one incremental pass against the same path and database.

### PGO Comparison

Use `make compare-pgo` to build both variants and benchmark them against the same workload with `sudo`:

```bash
make compare-pgo
```

Useful knobs:

- `COMPARE_PGO_PATH=/path` chooses the benchmark target path. Default: `/`.
- `COMPARE_PGO_RUNS=3` sets the number of measured runs per binary. Default: `2`.
- `COMPARE_PGO_WARMUPS=1` sets the number of warm-up runs before measuring. Default: `1`.
- `COMPARE_PGO_USE_SUDO=true|false` controls whether the benchmark runs under `sudo`. Default: `true`.

This is the intended way to decide whether a future PGO-enabled build should be reintroduced.

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

## Configuration file

Indexer uses a stable JSON config file. Native systemd installs use `/etc/indexer/config.json`; LinuxServer.io-style containers should use `/config/indexer.json`, matching the usual persisted `/config` volume. You can override the path with `--config-file` or `INDEXER_CONFIG_FILE`.

Common settings:

```json
{
  "index_path": "/data",
  "index_name": "data",
  "include_hidden": false,
  "include_network_mounts": false,
  "fresh_index": true,
  "keep_indexes": 0,
  "db_path": "/tmp/indexer.db",
  "db_busy_timeout": "5s",
  "db_journal_mode": "WAL",
  "db_synchronous": "OFF",
  "db_auto_vacuum": "INCREMENTAL",
  "db_max_open_conns": 5,
  "db_max_idle_conns": 2,
  "db_conn_max_idle_time": "5m0s",
  "socket_path": "/var/run/indexer.sock",
  "listen_addr": "",
  "interval": "6h0m0s"
}
```

On daemon startup, values are applied in this order:

1. Built-in defaults
2. JSON config file
3. `INDEXER_*` environment overrides
4. CLI/systemd flags

The `db_journal_mode`, `db_synchronous`, `db_auto_vacuum`, and connection pool fields control SQLite open-time behavior. Defaults keep WAL enabled for concurrent readers while indexing.

Apply changes:

```bash
sudo indexer config apply
sudo indexer config set --interval 6h
sudo indexer config set --path "/media/My Drive"
```

You can also use the built-in setup wizard:

```bash
sudo indexer setup
```

For scripts or one-off edits, `indexer config set` updates the same file without prompts:

```bash
sudo indexer config set --path "/media/My Drive" --interval 6h
```

On native systemd installs, `indexer config set --interval ...` also updates `/etc/systemd/system/indexer-index.timer.d/override.conf` and restarts the timer. `--interval 0` disables the timer and removes it from `indexer.target` through a target drop-in. Socket path changes are applied through an `indexer.socket` drop-in and reflected in `indexer.target`. Use `indexer config apply` after manual JSON edits to regenerate those drop-ins. Add `--dry-run` to setup/config-set commands to print the resulting JSON file without writing it or applying systemd changes.

For an interactive terminal overview:

```bash
sudo indexer dashboard
```

The dashboard shows API status, index statistics, systemd unit state, and recent logs. It can also run confirmed actions such as starting `indexer-index.service`, applying config, stopping the disposable API daemon, restarting `indexer.target`, and triggering `/vacuum` or `/prune`.

The daemon also exposes `GET /config` and Unix-socket-only `PUT /config` for settings UIs. Socket permissions are the local guard; the packaged systemd socket uses `0660`.

Service files are available in the `systemd/` directory for reference.

## Performance

- Indexing speed: ~50k files/sec on SSD (warm cache)
- Memory usage: ~150 MB for ~1M files in streaming mode
- Native systemd installs keep no `indexer` process resident while the API is idle and no scheduled index job is running
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
