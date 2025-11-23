# Indexer Benchmark Results

**Generated:** 2025-11-23 09:07:48 UTC
**Host:** ubuntuserver
**CPU:** Intel(R) N100
**Memory:** 15Gi
**Kernel:** 6.8.0-88-generic

## Configuration

- **Test Path:** `/home/miguelmariz`
- **Database:** `/tmp/benchmark_indexer.db`
- **Include Hidden:** true

## Scenarios

- Fresh DB: index from scratch with no existing database
- Reindex (After Deletions): rerun after removing synthetic files (optional)
## Summary

- Fresh DB: 16.111s, idle 9.24 MB, max 161.55 MB, avg 93.42 MB, entries 993303 (dirs 238072, files 755231), DB 384M, cleaned 0
- Reindex (After Deletions): 16.104s, idle 9.25 MB, max 153.27 MB, avg 79.20 MB, entries 992303 (dirs 238072, files 754231), DB 384M, cleaned 1000

## Results

### Fresh DB
- Duration: 16.111s
- Idle Memory: 9.24 MB (after startup, before reindex)
- Max Memory: 161.55 MB
- Avg Memory: 93.42 MB
- Entries: 993303 (dirs 238072, files 755231)
- DB Size: 384M
- Deleted Entries: 0
- Snapshot Mem: VmRSS 22.75 MB (VmHWM 161.07 MB, VmPeak 1986.70 MB)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Reindex (After Deletions)
- Duration: 16.104s
- Idle Memory: 9.25 MB (after startup, before reindex)
- Max Memory: 153.27 MB
- Avg Memory: 79.20 MB
- Entries: 992303 (dirs 238072, files 754231)
- DB Size: 384M
- Deleted Entries: 1000
- Snapshot Mem: VmRSS 24.14 MB (VmHWM 162.83 MB, VmPeak 2058.77 MB)
- Snapshot File: /tmp/benchmark_Reindex__After_Deletions__top.txt

