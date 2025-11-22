# Indexer Benchmark Results

**Generated:** 2025-11-22 18:41:10 UTC
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

- Fresh DB: 12.085s, idle 9.30 MB, max 188.60 MB, avg 119.14 MB, entries 982016 (dirs 237704, files 744312), DB 380M, cleaned 0
- Reindex (After Deletions): 12.074s, idle 9.30 MB, max 187.63 MB, avg 117.12 MB, entries 981016 (dirs 237704, files 743312), DB 380M, cleaned 1000

## Results

### Fresh DB
- Duration: 12.085s
- Idle Memory: 9.30 MB (after startup, before reindex)
- Max Memory: 188.60 MB
- Avg Memory: 119.14 MB
- Entries: 982016 (dirs 237704, files 744312)
- DB Size: 380M
- Deleted Entries: 0
- Snapshot Mem: VmRSS 185.29 MB (VmHWM 188.43 MB, VmPeak 2061.61 MB)
- Snapshot File: /tmp/benchmark_Fresh_DB_top.txt

### Reindex (After Deletions)
- Duration: 12.074s
- Idle Memory: 9.30 MB (after startup, before reindex)
- Max Memory: 187.63 MB
- Avg Memory: 117.12 MB
- Entries: 981016 (dirs 237704, files 743312)
- DB Size: 380M
- Deleted Entries: 1000
- Snapshot Mem: VmRSS 180.79 MB (VmHWM 187.34 MB, VmPeak 2061.29 MB)
- Snapshot File: /tmp/benchmark_Reindex__After_Deletions__top.txt

