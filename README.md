# Indexer

Indexer is a Go-based filesystem crawler that snapshots directory trees into a SQLite database. It is designed to power LinuxIO, storing both directories and files (with inode metadata) so the UI and other services can answer questions such as “How big is `/home`?” or “List files under `/var/log`” directly from SQLite without repeated disk scans. The implementation is Linux-only by design.

This code is inspired by the code in Filebrowser Quantum - https://github.com/gtsteffaniak/filebrowser

## Features

- Recursive indexing of any path with optional hidden-file support.
- Automatic detection of hard links and de-duplication of disk usage totals.
- Persistence to SQLite using the [`mattn/go-sqlite3`](https://github.com/mattn/go-sqlite3) driver.
- Snapshot “resume” mode for quick re-indexing of unchanged trees.
- Refresh mode for single-file or directory updates (ideal after copy/move/delete operations).
- Systemd service/timer definitions for scheduled scans.
- Built-in rate limiter to avoid repeated full scans in short succession.

## Building

```bash
go version   # ensure Go >= 1.20
go mod download
make build   # writes ./indexer
```

CI builds run automatically on every push & PR via `.github/workflows/build.yml`. Each run uploads a prebuilt Linux AMD64 binary (`indexer-linux-amd64`) as an artifact you can download from the workflow summary. Use those artifacts to publish releases or distribute builds without compiling locally.

Tests:

```bash
go test ./...
```

## CLI Usage

The binary uses a subcommand-based CLI:

```bash
indexer <command> [options]
```

### Commands

1. **index** – full scan + write to DB:

```bash
indexer index \
  -path /home/miguelmariz \
  -name my_home \
  -include-hidden \
  -db-path /var/lib/linuxIO/indexer.db \
  -resume
```

Key flags:

- `-path` (required): filesystem root to scan.
- `-name`: index name (defaults to sanitized `-path`).
- `-include-hidden`: include dotfiles/dotdirs.
- `-db-path`: SQLite file; defaults to `INDEXER_DB_PATH` env or `indexer.db`.
- `-resume`: preload the last snapshot from SQLite to enable quick scans.
- `-no-rate-limit`: disable the 30-second rate limiter for full scans.

2. **refresh** – refresh specific paths in an existing index:

```bash
indexer refresh \
  -name my_home \
  -db-path /var/lib/linuxIO/indexer.db \
  -refresh-path /home/miguelmariz/newfile.txt \
  -refresh-recursive
```

- `-name` (required): index name in the database.
- `-db-path`: same as above.
- `-refresh-path`: repeatable flag for absolute paths to refresh.
- `-refresh-recursive`: when refreshing directories, also re-index their subtrees.

3. **search** – search using an existing snapshot (no new scan):

```bash
indexer search \
  -db-path /var/lib/linuxIO/indexer.db \
  -name my_home \
  -query log \
  -case-sensitive=false \
  -json
```

- `-name`: index name (optional when only one index exists in the DB; otherwise required).
- `-db-path`: SQLite file.
- `-query`: search term; if omitted, the first positional argument is treated as the query (for example `indexer search log`).
- `-case-sensitive`: toggle case sensitivity.
- `-json`: return JSON instead of log-style text.

4. **serve** – run a small HTTP API backed by SQLite:

```bash
indexer serve \
  -db-path /var/lib/linuxIO/indexer.db \
  -addr :8080 \
  -default-index my_home
```

Endpoints (read-only, JSON, no external libraries):

- `GET /health` – simple health check.
- `GET /search?q=<term>&name=<index>&caseSensitive=true|false` – search an index using the same semantics as the CLI `search` command. If `name` is omitted, `-default-index` is used, or a single index is auto-detected.
- `GET /stats?path=<dir>&name=<index>` – return aggregate directory statistics (size, recursive file/dir counts, last modified) for a given path. If `name` is omitted, `-default-index` is used, or a single index is auto-detected.
- `GET /size?path=<dir>&name=<index>` – return only the total size (bytes) for a directory inside the index. If `name` is omitted, `-default-index` is used (or a single index is auto-detected).

5. **stats** – aggregate directory statistics (fast, DB-only):

```bash
indexer stats \
  -db-path /var/lib/linuxIO/indexer.db \
  -name my_home \
  -path /var/log

# or with a positional path:
indexer stats /var/log
```

Returns total size, recursive file/dir counts, and last modified time for the given directory path inside the index. `-name` is optional when only one index exists; otherwise it is required.

6. **size** – just the total size of a directory (fast, DB-only):

```bash
indexer size /var/log

# multiple paths:
indexer size /home /tmp /var/log
```

- Uses the default index when only one exists in the DB (otherwise `-name` is required).
- For a single path, prints just the directory size in bytes. For multiple paths, prints one line per path as `<normalized-path> <size-bytes>`. All sizes are pre-aggregated during indexing.

Rate limiting prevents `indexer index` full scans from starting more than once every 30 seconds per DB path. If a run happens too soon, the CLI exits with a “retry in …” message.

## Database Layout & API

The SQLite database has two primary tables:

- `indexes`: metadata per index (name, root_path, counts, disk usage, timings, include-hidden flag, etc.).
- `entries`: flattened rows for every file/directory (`relative_path`, `absolute_path`, size, mod_time, type, hidden, inode, is_dir).

Useful queries:

```sql
-- directory size / du equivalent
SELECT relative_path,
       size,
       round(size / 1024.0 / 1024, 2) AS size_mb
FROM entries
WHERE is_dir = 1 AND relative_path = '/home';

-- list largest folders under /home
SELECT relative_path,
       round(size / 1024.0 / 1024, 2) AS size_mb
FROM entries
WHERE is_dir = 1 AND relative_path LIKE '/home/%'
ORDER BY size DESC
LIMIT 20;

-- sample files (logical size + inode)
SELECT relative_path, size, inode
FROM entries
WHERE is_dir = 0 AND relative_path LIKE '/var/log/%'
LIMIT 50;
```

Because we store inode numbers, you can detect hard links or consolidate duplicates (`GROUP BY inode`). Directory rows already reflect de-duplicated disk usage (matching `du` when run with the same permissions).

Timing metrics (`index_duration_ms`, `export_duration_ms`, `vacuum_duration_ms`) live on the `indexes` table for performance monitoring.

## Refresh Workflow (API Hook)

External tools can invoke incremental updates via the `refresh` command (or by calling the HTTP API and then triggering `indexer refresh` through systemd or another orchestrator):

- For file changes: pass the file path (the parent directory is refreshed automatically).
- For directory changes: pass the directory path; include `-refresh-recursive` if the entire subtree is new or modified.
- For moves/renames: refresh both source and destination paths.

This workflow only touches the specified directories and persists the snapshot again, keeping the DB in sync without re-walking the entire filesystem.

## Notes

- Hidden files are skipped unless `-include-hidden` is set.
- The indexer runs with the permissions of the invoking user; to capture system-wide paths, run it as root.
- If the filesystem changes during a scan, a subsequent pass (full or refresh) will reconcile the DB.
- Internal index paths are canonicalized as Linux-style directory paths (always a leading `/` and a trailing `/` for directories, for example `/` or `/var/log/`); other platforms are not supported.

Feel free to tailor the flags, systemd units, and queries to your environment. Contributions and issues are welcome!
