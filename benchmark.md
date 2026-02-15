# Indexer Benchmark Results

**Generated:** 2026-02-15 19:37:02 UTC
**Host:** ubuntuserver
**CPU:** Intel(R) N100
**Memory:** 15Gi
**Kernel:** 6.8.0-100-generic

## Configuration

- **Test Path:** `/`
- **Database:** `/tmp/benchmark_indexer.db`
- **Include Hidden:** true

## Scenarios

- Fresh DB: index from scratch with no existing database
- Index (After Deletions): rerun after removing synthetic files (optional)
## Summary

- Fresh DB: 17.236s, idle 0.00 MB, max 67.04 MB, avg 39.70 MB, entries 1247600 (dirs 279093, files 968507), DB 523M, cleaned 0
- Index (After Deletions): 17.223s, idle 0.00 MB, max 56.29 MB, avg 33.99 MB, entries 1246603 (dirs 279093, files 967510), DB 524M, cleaned 0

## Results

### Fresh DB
- Duration: 17.236s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 67.04 MB
- Avg Memory: 39.70 MB
- Entries: 1247600 (dirs 279093, files 968507)
- DB Size: 523M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Index (After Deletions)
- Duration: 17.223s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 56.29 MB
- Avg Memory: 33.99 MB
- Entries: 1246603 (dirs 279093, files 967510)
- DB Size: 524M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Index__After_Deletions__top.txt

