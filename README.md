# Indexer

Indexer is a Go-based filesystem crawler that snapshots directory trees into a SQLite database. It is designed to power LinuxIO, storing both directories and files (with inode metadata) so the UI and other services can answer questions such as “How big is `/home`?” or “List files under `/var/log`” directly from SQLite without repeated disk scans.

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
make build   # writes ./bin/indexer
```

CI builds run automatically on every push & PR via `.github/workflows/build.yml`. Each run uploads a prebuilt Linux AMD64 binary (`indexer-linux-amd64`) as an artifact you can download from the workflow summary. Use those artifacts to publish releases or distribute builds without compiling locally.

Tests:

```bash
go test ./...
```

## Publishing Binaries

1. Trigger a build (push to `main`, open/merge a PR, or use the “Run workflow” button).
2. Once the GitHub Action finishes, download the `indexer-linux-amd64` artifact from the run’s “Artifacts” section.
3. Rename/sign/compress as desired (e.g., `indexer-v1.0.0-linux-amd64`) and attach it to a GitHub Release or distribute it directly.

Artifacts are produced with `go build -o dist/indexer-linux-amd64 .`; if you need additional targets (ARM, macOS), extend the workflow with a matrix or run `GOOS/GOARCH` builds locally.

## CLI Usage

```
indexer \
  -path /home/miguelmariz \
  -name my_home \
  -include-hidden \
  -db-path /var/lib/linuxIO/indexer.db \
  -resume
```

### Global Flags

| Flag | Description |
| ---- | ----------- |
| `-path` (required) | Filesystem root to scan. |
| `-name` | Index name (defaults to sanitized `-path`). |
| `-include-hidden` | Include dotfiles/dotdirs. |
| `-db-path` | SQLite file; defaults to `INDEXER_DB_PATH` env or `indexer.db`. |
| `-resume` | Preload the last snapshot from SQLite to enable quick scans. |
| `-refresh-path` | Repeatable flag to refresh one or more absolute paths instead of running a full scan. |
| `-refresh-recursive` | When refreshing directories, also re-index their subtrees. |

### Modes

1. **Full Scan (default)** – Reads the entire tree, then writes a fresh snapshot to SQLite. When `-resume` is provided and a snapshot exists, unchanged directories are skipped automatically.
2. **Refresh Mode** – Triggered when one or more `-refresh-path` values are supplied. The program loads the snapshot, re-indexes only the specified paths, and persists again. Ideal after targeted file operations (copy/move/delete) without running a full scan.

Rate limiting prevents full scans from starting more than once every 30 seconds (per DB path). If a run happens too soon, the CLI exits with a “retry in …” message.

## Systemd Integration

Unit files live under `systemd/`:

- `linuxio-indexer.service`
- `linuxio-indexer.timer`

Install and enable:

```bash
sudo install -m644 systemd/linuxio-indexer.service /etc/systemd/system/
sudo install -m644 systemd/linuxio-indexer.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now linuxio-indexer.timer
```

The service runs `/home/miguelmariz/indexer/bin/indexer -path / -include-hidden -db-path /var/lib/linuxIO/indexer.db -resume` as root every 15 minutes. Adjust the unit to match your binary location or flags.

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

External tools (like LinuxIO) can invoke incremental updates:

```bash
/home/miguelmariz/indexer/bin/indexer \
  -path /home/miguelmariz \
  -db-path /var/lib/linuxIO/indexer.db \
  -refresh-path /home/miguelmariz/newfile.txt
```

- For file changes: pass the file path (the parent directory is refreshed automatically).
- For directory changes: pass the directory path; include `-refresh-recursive` if the entire subtree is new or modified.
- For moves/renames: refresh both source and destination paths.

This workflow only touches the specified directories and persists the snapshot again, keeping the DB in sync without re-walking the entire filesystem.

## Notes

- Hidden files are skipped unless `-include-hidden` is set.
- The indexer runs with the permissions of the invoking user; to capture system-wide paths, run it as root.
- If the filesystem changes during a scan, a subsequent pass (full or refresh) will reconcile the DB.

Feel free to tailor the flags, systemd units, and queries to your environment. Contributions and issues are welcome!
