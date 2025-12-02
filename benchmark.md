# Indexer Benchmark Results

**Generated:** 2025-12-02 11:07:21 UTC
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
- Index (After Deletions): rerun after removing synthetic files (optional)
## Summary

- Fresh DB: 11.142s, idle 0.00 MB, max 43.24 MB, avg 38.33 MB, entries 1214810 (dirs 264431, files 950379), DB 473M, cleaned 0
- Index (After Deletions): 12.658s, idle 0.00 MB, max 67.91 MB, avg 35.48 MB, entries 1213811 (dirs 264431, files 949380), DB 473M, cleaned 0

## Results

### Fresh DB
- Duration: 11.142s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 43.24 MB
- Avg Memory: 38.33 MB
- Entries: 1214810 (dirs 264431, files 950379)
- DB Size: 473M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Index (After Deletions)
- Duration: 12.658s
- Idle Memory: 0.00 MB (after startup, before index)
- Max Memory: 67.91 MB
- Avg Memory: 35.48 MB
- Entries: 1213811 (dirs 264431, files 949380)
- DB Size: 473M
- Deleted Entries: 0
- Snapshot Mem: VmRSS n/a (VmHWM n/a, VmPeak n/a)
- Snapshot File: /tmp/benchmark_Index__After_Deletions__top.txt

