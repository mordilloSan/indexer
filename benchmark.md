# Indexer Benchmark Results

**Generated:** 2025-11-24 13:41:08 UTC
**Host:** ubuntuserver
**CPU:** Intel(R) N100
**Memory:** 15Gi
**Kernel:** 6.8.0-88-generic

## Configuration

- **Test Path:** `/`
- **Database:** `/tmp/benchmark_indexer.db`
- **Include Hidden:** true

## Scenarios

- Fresh DB: index from scratch with no existing database
- Reindex (After Deletions): rerun after removing synthetic files (optional)
## Summary

- Fresh DB: 13.664s, idle 0.00 MB, max 67.01 MB, avg 39.61 MB, entries 1238796 (dirs 264710, files 974086), DB 481M, cleaned 0
- Reindex (After Deletions): 20.778s, idle 0.00 MB, max 54.43 MB, avg 37.13 MB, entries 1237797 (dirs 264710, files 973087), DB 481M, cleaned 0

## Results

### Fresh DB
- Duration: 13.664s
- Idle Memory: 0.00 MB (after startup, before reindex)
- Max Memory: 67.01 MB
- Avg Memory: 39.61 MB
- Entries: 1238796 (dirs 264710, files 974086)
- DB Size: 481M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Reindex (After Deletions)
- Duration: 20.778s
- Idle Memory: 0.00 MB (after startup, before reindex)
- Max Memory: 54.43 MB
- Avg Memory: 37.13 MB
- Entries: 1237797 (dirs 264710, files 973087)
- DB Size: 481M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Reindex__After_Deletions__top.txt

