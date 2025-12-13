# Indexer Benchmark Results

**Generated:** 2025-12-13 12:31:34 UTC
**Host:** ubuntuserver
**CPU:** Intel(R) N100
**Memory:** 15Gi
**Kernel:** 6.8.0-90-generic

## Configuration

- **Test Path:** `/`
- **Database:** `/tmp/benchmark_indexer.db`
- **Include Hidden:** true

## Scenarios

- Fresh DB: index from scratch with no existing database
- Index (After Deletions): rerun after removing synthetic files (optional)
## Summary

- Fresh DB: 11.703s, idle 0.00 MB, max 47.80 MB, avg 39.01 MB, entries 1065195 (dirs 235828, files 829367), DB 416M, cleaned 0
- Index (After Deletions): 11.143s, idle 0.00 MB, max 44.51 MB, avg 34.09 MB, entries 1064113 (dirs 235827, files 828286), DB 416M, cleaned 0

## Results

### Fresh DB
- Duration: 11.703s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 47.80 MB
- Avg Memory: 39.01 MB
- Entries: 1065195 (dirs 235828, files 829367)
- DB Size: 416M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Index (After Deletions)
- Duration: 11.143s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 44.51 MB
- Avg Memory: 34.09 MB
- Entries: 1064113 (dirs 235827, files 828286)
- DB Size: 416M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Index__After_Deletions__top.txt

