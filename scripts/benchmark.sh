#!/bin/bash

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Configuration
INDEXER_PATH="${INDEXER_PATH:-$PROJECT_ROOT/indexer}"
TEST_PATH="${TEST_PATH:-/}"
DB_PATH="${DB_PATH:-/tmp/benchmark_indexer.db}"
SOCKET_PATH="${SOCKET_PATH:-/tmp/benchmark_indexer.sock}"
LISTEN_ADDR="${LISTEN_ADDR:-}"
INCLUDE_HIDDEN="${INCLUDE_HIDDEN:-true}"
OUTPUT_FILE="${OUTPUT_FILE:-benchmark.md}"
RUN_INDEX="${RUN_INDEX:-true}"
TOP_SNAPSHOT="${TOP_SNAPSHOT:-true}"
DATASET_DIR="${DATASET_DIR:-}"
DATASET_FILES="${DATASET_FILES:-1000}"
GOCACHE="${GOCACHE:-$PROJECT_ROOT/.cache}"
REMOVE_GOCACHE_ON_EXIT=false
if [ "$GOCACHE" = "$PROJECT_ROOT/.cache" ]; then
    REMOVE_GOCACHE_ON_EXIT=true
fi

curl_api() {
    local path=$1
    local method=${2:-GET}
    local data=${3:-}
    if [ -n "$LISTEN_ADDR" ]; then
        if [ -n "$data" ]; then
            curl -sf -X "$method" "http://localhost${LISTEN_ADDR}${path}" -d "$data"
        else
            curl -sf -X "$method" "http://localhost${LISTEN_ADDR}${path}"
        fi
    else
        if [ -n "$data" ]; then
            curl -sf --unix-socket "$SOCKET_PATH" -X "$method" "http://localhost${path}" -d "$data"
        else
            curl -sf --unix-socket "$SOCKET_PATH" -X "$method" "http://localhost${path}"
        fi
    fi
}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_step() {
    echo -e "\n${BLUE}==>${NC} $1\n"
}

GO_BIN="${GO_BIN:-}"
if [ -z "$GO_BIN" ] && command -v go >/dev/null 2>&1; then
    GO_BIN=$(command -v go)
fi
if [ -z "$GO_BIN" ]; then
    for candidate in /usr/local/go/bin/go /usr/local/bin/go /usr/bin/go; do
        if [ -x "$candidate" ]; then
            GO_BIN="$candidate"
            break
        fi
    done
fi
if [ -z "$GO_BIN" ]; then
    log_error "Go binary not found. Set GO_BIN or ensure go is on PATH."
    exit 1
fi
log_info "Using Go binary at $GO_BIN"

prepare_dataset() {
    mkdir -p "$TEST_PATH" 2>/dev/null || {
        log_error "Unable to create TEST_PATH ($TEST_PATH); check permissions"
        exit 1
    }
    if [ -z "$DATASET_DIR" ]; then
        local base="$TEST_PATH"
        if [ "$TEST_PATH" = "/" ]; then
            base="/tmp"
        fi
        DATASET_DIR="$(mktemp -d "$base/.indexer_benchmark_data.XXXXXX")"
    fi
    if [ "$DATASET_DIR" = "/" ] || [ -z "$DATASET_DIR" ]; then
        log_error "Refusing to use unsafe DATASET_DIR: '$DATASET_DIR'"
        exit 1
    fi
    if [ "$TEST_PATH" != "/" ]; then
        case "$DATASET_DIR" in
            "$TEST_PATH"/*|"$TEST_PATH") ;;
            *)
                log_error "DATASET_DIR must live under TEST_PATH (current: $DATASET_DIR, TEST_PATH: $TEST_PATH)"
                exit 1
                ;;
        esac
    fi
    log_step "Preparing synthetic dataset ($DATASET_FILES files) at $DATASET_DIR"
    if [ -e "$DATASET_DIR" ]; then
        rm -rf "$DATASET_DIR" || {
            log_error "Failed to clean DATASET_DIR ($DATASET_DIR); check ownership/permissions"
            exit 1
        }
    fi
    mkdir -p "$DATASET_DIR"
    for i in $(seq 1 "$DATASET_FILES"); do
        printf "benchmark file %04d\n" "$i" > "$DATASET_DIR/file_$i.txt"
    done
    log_info "Created $DATASET_FILES files"
}

mutate_dataset_for_index() {
    if [ "$DATASET_DIR" = "/" ] || [ -z "$DATASET_DIR" ]; then
        log_error "Refusing to mutate unsafe DATASET_DIR: '$DATASET_DIR'"
        exit 1
    fi
    if [ "$TEST_PATH" != "/" ]; then
        case "$DATASET_DIR" in
            "$TEST_PATH"/*|"$TEST_PATH") ;;
            *)
                log_error "DATASET_DIR must live under TEST_PATH (current: $DATASET_DIR, TEST_PATH: $TEST_PATH)"
                exit 1
                ;;
        esac
    fi
    log_step "Mutating dataset for index (deleting files)"
    find "$DATASET_DIR" -maxdepth 1 -type f -delete
    log_info "Deleted files in $DATASET_DIR"
}

get_rss_kb() {
    local pid=$1
    if [ -r "/proc/$pid/smaps_rollup" ]; then
        awk '/^Rss:/ {print $2}' "/proc/$pid/smaps_rollup" 2>/dev/null
        return
    fi
    if [ -r "/proc/$pid/statm" ]; then
        local rss_pages page_size
        rss_pages=$(awk 'NR==1 {print $2}' "/proc/$pid/statm" 2>/dev/null)
        page_size=$(getconf PAGESIZE 2>/dev/null || echo 4096)
        if [ -n "$rss_pages" ]; then
            echo $((rss_pages * page_size / 1024))
            return
        fi
    fi
    ps -o rss= -p "$pid" 2>/dev/null | awk 'NR==1 {print $1}'
}

capture_top_snapshot() {
    local pid=$1
    local outfile=$2
    if ! kill -0 "$pid" 2>/dev/null; then
        echo "process $pid not running at snapshot time" > "$outfile"
        return
    fi
    {
        echo "== top (pid $pid) =="
        if command -v top >/dev/null 2>&1; then
            top -b -n1 -p "$pid" 2>/dev/null || true
        else
            echo "top not available; showing ps"
            ps -p "$pid" -o pid,ppid,%mem,%cpu,rss,vsz,cmd 2>/dev/null || true
        fi
        echo ""
        if [ -r "/proc/$pid/status" ]; then
            echo "== /proc/$pid/status =="
            grep -E '^(VmPeak|VmSize|VmRSS|VmHWM|Threads)' "/proc/$pid/status" 2>/dev/null || true
        fi
        if [ -r "/proc/$pid/smaps_rollup" ]; then
            echo ""
            echo "== /proc/$pid/smaps_rollup (first lines) =="
            head -n 20 "/proc/$pid/smaps_rollup" 2>/dev/null || true
        fi
    } > "$outfile"
}

# Build indexer if needed
build_indexer() {
    log_step "Building indexer"
    mkdir -p "$GOCACHE" 2>/dev/null || true
    (cd "$PROJECT_ROOT" && GOCACHE="$GOCACHE" "$GO_BIN" build -o "$INDEXER_PATH" .)
    log_info "Built indexer at $INDEXER_PATH"
}

# Monitor memory usage of a process
monitor_memory() {
    local pid=$1
    local output_file=$2
    local max_mem=0
    local avg_mem=0
    local samples=0

    sample_mem() {
        local mem
        mem=$(get_rss_kb "$pid")
        if [ -n "$mem" ] && [ "$mem" != "0" ]; then
            samples=$((samples + 1))
            avg_mem=$((avg_mem + mem))
            if [ "$mem" -gt "$max_mem" ]; then
                max_mem=$mem
            fi
        fi
    }

    # Always take an initial sample
    sample_mem

    while kill -0 "$pid" 2>/dev/null; do
        sleep 0.5
        sample_mem
    done

    if [ "$samples" -gt 0 ]; then
        avg_mem=$((avg_mem / samples))
    fi

    echo "max:$max_mem avg:$avg_mem samples:$samples" > "$output_file"
}

# Run indexer and measure performance
run_benchmark() {
    local scenario=$1
    local db_exists=$2
    local safe_scenario
    safe_scenario=$(echo "$scenario" | tr ' /()' '__')

    log_step "Running benchmark: $scenario"

    # Clean up previous run if fresh start
    if [ "$db_exists" = "false" ]; then
        rm -f "$DB_PATH"* "$SOCKET_PATH"
        log_info "Cleaned up previous database"
    fi

    local hidden_flag=""
    if [ "$INCLUDE_HIDDEN" = "true" ]; then
        hidden_flag="--include-hidden"
    fi

    local log_file="/tmp/benchmark_${safe_scenario}.log"
    local cmd=( "$INDEXER_PATH" --index-mode --path "$TEST_PATH" --db-path "$DB_PATH" $hidden_flag --verbose )

    log_info "Starting direct index: ${cmd[*]}"

    local start_time
    start_time=$(date +%s.%N)

    "${cmd[@]}" > "$log_file" 2>&1 &
    local indexer_pid=$!

    # Start memory monitoring in background
    local mem_file="/tmp/benchmark_${safe_scenario}_mem.txt"
    monitor_memory "$indexer_pid" "$mem_file" &
    local monitor_pid=$!

    # Wait for completion
    wait "$indexer_pid"
    local exit_code=$?
    wait "$monitor_pid" 2>/dev/null || true

    local end_time
    end_time=$(date +%s.%N)
    local duration
    duration=$(awk -v start="$start_time" -v end="$end_time" 'BEGIN { printf "%.3f", end - start }')

    if [ $exit_code -ne 0 ]; then
        log_error "Index process failed (exit $exit_code); see $log_file"
        return 1
    fi

    # Optional snapshot of the live process before it exits (best-effort)
    local top_file="/tmp/benchmark_${safe_scenario}_top.txt"
    if [ "$TOP_SNAPSHOT" = "true" ]; then
        capture_top_snapshot "$indexer_pid" "$top_file"
    fi

    # Collect results
    local mem_stats
    mem_stats=$(cat "$mem_file" 2>/dev/null || echo "max:0 avg:0 samples:0")
    local max_mem
    max_mem=$(echo "$mem_stats" | grep -o 'max:[0-9]*' | cut -d':' -f2)
    local avg_mem
    avg_mem=$(echo "$mem_stats" | grep -o 'avg:[0-9]*' | cut -d':' -f2)
    local num_samples
    num_samples=$(echo "$mem_stats" | grep -o 'samples:[0-9]*' | cut -d':' -f2)

    # Convert KB to MB
    local max_mem_mb
    max_mem_mb=$(awk -v m="$max_mem" 'BEGIN { printf "%.2f", m / 1024 }')
    local avg_mem_mb
    avg_mem_mb=$(awk -v m="$avg_mem" 'BEGIN { printf "%.2f", m / 1024 }')
    local idle_mem_mb="0.00"

    # Get database stats
    local db_size
    db_size=$(du -h "$DB_PATH" 2>/dev/null | cut -f1)
    local num_entries
    num_entries=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM entries;" 2>/dev/null || echo "0")
    local num_dirs
    num_dirs=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM entries WHERE type = 'directory';" 2>/dev/null || echo "0")
    local num_files
    num_files=$(sqlite3 "$DB_PATH" "SELECT COUNT(*) FROM entries WHERE type = 'file';" 2>/dev/null || echo "0")

    # Check for cleanup in logs
    local deleted_entries="0"

    log_info "Duration: ${duration}s"
    log_info "Max Memory: ${max_mem_mb} MB (from ${num_samples} samples)"
    log_info "Avg Memory: ${avg_mem_mb} MB"
    log_info "Entries: $num_entries (dirs: $num_dirs, files: $num_files)"
    if [ "$deleted_entries" != "0" ]; then
        log_info "Deleted entries: $deleted_entries"
    fi
    if [ "$TOP_SNAPSHOT" = "true" ] && [ -s "$top_file" ]; then
        log_info "Top snapshot saved: $top_file"
    fi

    if [ "${num_entries:-0}" -eq 0 ]; then
        log_error "No entries indexed; leaving logs at $log_file"
        log_error "DB path: $DB_PATH"
        return 1
    fi

    # Store results
    echo "$scenario|$duration|$idle_mem_mb|$max_mem_mb|$avg_mem_mb|$num_entries|$num_dirs|$num_files|$db_size|$deleted_entries" >> /tmp/benchmark_results.txt

    # Clean up
    rm -f "$mem_file"
}

# Generate markdown report
generate_report() {
    log_step "Generating report: $OUTPUT_FILE"

    local timestamp
    timestamp=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
    local hostname
    hostname=$(hostname)
    local cpu_info
    cpu_info=$(grep "model name" /proc/cpuinfo | head -1 | cut -d':' -f2 | xargs)
    local mem_total
    mem_total=$(free -h | grep Mem: | awk '{print $2}')
    local kernel
    kernel=$(uname -r)

    cat > "$OUTPUT_FILE" << EOF
# Indexer Benchmark Results

**Generated:** $timestamp
**Host:** $hostname
**CPU:** $cpu_info
**Memory:** $mem_total
**Kernel:** $kernel

## Configuration

- **Test Path:** \`$TEST_PATH\`
- **Database:** \`$DB_PATH\`
- **Include Hidden:** $INCLUDE_HIDDEN

## Scenarios

- Fresh DB: index from scratch with no existing database
- Index (After Deletions): rerun after removing synthetic files (optional)
EOF

    local results
    results=$(cat /tmp/benchmark_results.txt)

    {
    echo "## Summary"
    echo ""
    while IFS='|' read -r scenario duration idle_mem max_mem avg_mem total dirs files db_size deleted; do
        echo "- $scenario: ${duration}s, idle ${idle_mem} MB, max ${max_mem} MB, avg ${avg_mem} MB, entries $total (dirs $dirs, files $files), DB $db_size, cleaned $deleted"
    done <<< "$results"
    echo ""
    echo "## Results"
    echo ""
    while IFS='|' read -r scenario duration idle_mem max_mem avg_mem total dirs files db_size deleted; do
        echo "### $scenario"
        echo "- Duration: ${duration}s"
        echo "- Idle Memory: ${idle_mem} MB (after startup, before index)"
        echo "- Max Memory: ${max_mem} MB"
        echo "- Avg Memory: ${avg_mem} MB"
        echo "- Entries: $total (dirs $dirs, files $files)"
        echo "- DB Size: $db_size"
        echo "- Deleted Entries: $deleted"
        if [ "$TOP_SNAPSHOT" = "true" ]; then
            local safe_scenario
            safe_scenario=$(echo "$scenario" | tr ' /()' '__')
            local top_file="/tmp/benchmark_${safe_scenario}_top.txt"
            if [ -s "$top_file" ]; then
                local vmrss vmhwm vmpeak
                vmrss=$(grep -m1 '^VmRSS:' "$top_file" | awk '{print $2}')
                vmhwm=$(grep -m1 '^VmHWM:' "$top_file" | awk '{print $2}')
                vmpeak=$(grep -m1 '^VmPeak:' "$top_file" | awk '{print $2}')
                if [ -n "$vmrss" ]; then
                    vmrss=$(awk -v k="$vmrss" 'BEGIN { printf "%.2f MB", k / 1024 }')
                fi
                if [ -n "$vmhwm" ]; then
                    vmhwm=$(awk -v k="$vmhwm" 'BEGIN { printf "%.2f MB", k / 1024 }')
                    fi
                    if [ -n "$vmpeak" ]; then
                        vmpeak=$(awk -v k="$vmpeak" 'BEGIN { printf "%.2f MB", k / 1024 }')
                    fi
                    echo "- Snapshot Mem: VmRSS ${vmrss:-n/a} (VmHWM ${vmhwm:-n/a}, VmPeak ${vmpeak:-n/a})"
                    echo "- Snapshot File: $top_file"
                else
                    echo "- Top Snapshot: (not captured)"
                fi
            fi
            echo ""
        done <<< "$results"
    } >> "$OUTPUT_FILE"

    log_info "Report generated: $OUTPUT_FILE"
}

# Main execution
main() {
    log_info "Starting benchmark suite"
    log_info "Test path: $TEST_PATH"

    # Clean up previous results
    rm -f /tmp/benchmark_results.txt

    prepare_dataset

    # Build
    build_indexer

    # Scenario 1: Fresh DB
    run_benchmark "Fresh DB" false

    # Scenario 2: Index (after deletions)
    if [ "$RUN_INDEX" = "true" ]; then
        mutate_dataset_for_index
        run_benchmark "Index (After Deletions)" true
    else
        log_warn "Skipping index-after-deletions scenario (set RUN_INDEX=true to enable)"
    fi

    # Generate report
    generate_report

    # Final cleanup
    rm -f "$DB_PATH"* "$SOCKET_PATH" /tmp/benchmark_results.txt
    rm -rf "$DATASET_DIR"
    if [ "$REMOVE_GOCACHE_ON_EXIT" = true ] && [ -d "$GOCACHE" ]; then
        rm -rf "$GOCACHE"
    fi

    log_info "Benchmark complete!"
    log_info "Results saved to: $OUTPUT_FILE"
}

# Run main
main "$@"
