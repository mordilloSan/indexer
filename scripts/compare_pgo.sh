#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

GO_BIN="${GO_BIN:-$(command -v go || true)}"
COMPARE_PGO_PATH="${COMPARE_PGO_PATH:-/}"
COMPARE_PGO_RUNS="${COMPARE_PGO_RUNS:-2}"
COMPARE_PGO_WARMUPS="${COMPARE_PGO_WARMUPS:-1}"
COMPARE_PGO_INCLUDE_HIDDEN="${COMPARE_PGO_INCLUDE_HIDDEN:-true}"
COMPARE_PGO_USE_SUDO="${COMPARE_PGO_USE_SUDO:-true}"
COMPARE_PGO_GOAMD64="${COMPARE_PGO_GOAMD64:-v3}"
COMPARE_PGO_BUILD_FLAGS_BASE="${COMPARE_PGO_BUILD_FLAGS_BASE:- -trimpath -buildvcs=false}"
COMPARE_PGO_LDFLAGS="${COMPARE_PGO_LDFLAGS:--s -w}"
NO_PGO_BIN="${NO_PGO_BIN:-/tmp/indexer-no-pgo}"
WITH_PGO_BIN="${WITH_PGO_BIN:-/tmp/indexer-pgo}"
WORK_ROOT="${WORK_ROOT:-$(mktemp -d /tmp/indexer-compare-pgo.XXXXXX)}"
KEEP_WORK_ROOT="${KEEP_WORK_ROOT:-false}"

if [ -z "$GO_BIN" ]; then
    echo "go binary not found on PATH; set GO_BIN explicitly" >&2
    exit 1
fi

if [ ! -f "$PROJECT_ROOT/default.pgo" ]; then
    echo "default.pgo not found in $PROJECT_ROOT; run 'make pgo' first" >&2
    exit 1
fi

cleanup() {
    if [ "$KEEP_WORK_ROOT" != "true" ] && [ -d "$WORK_ROOT" ]; then
        rm -rf "$WORK_ROOT"
    fi
}
trap cleanup EXIT

log() {
    printf '[compare-pgo] %s\n' "$1"
}

run_cmd() {
    if [ "$COMPARE_PGO_USE_SUDO" = "true" ]; then
        sudo "$@"
        return
    fi
    "$@"
}

validate_inputs() {
    if [ ! -d "$COMPARE_PGO_PATH" ]; then
        echo "COMPARE_PGO_PATH does not exist or is not a directory: $COMPARE_PGO_PATH" >&2
        exit 1
    fi
    if ! [[ "$COMPARE_PGO_RUNS" =~ ^[0-9]+$ ]] || [ "$COMPARE_PGO_RUNS" -lt 1 ]; then
        echo "COMPARE_PGO_RUNS must be an integer >= 1" >&2
        exit 1
    fi
    if ! [[ "$COMPARE_PGO_WARMUPS" =~ ^[0-9]+$ ]]; then
        echo "COMPARE_PGO_WARMUPS must be an integer >= 0" >&2
        exit 1
    fi
    case "$COMPARE_PGO_USE_SUDO" in
        true|false) ;;
        *)
            echo "COMPARE_PGO_USE_SUDO must be true or false" >&2
            exit 1
            ;;
    esac
}

build_binaries() {
    log "Building no-PGO binary: $NO_PGO_BIN"
    (
        cd "$PROJECT_ROOT"
        GOAMD64="$COMPARE_PGO_GOAMD64" \
            "$GO_BIN" build $COMPARE_PGO_BUILD_FLAGS_BASE -pgo=off \
            -ldflags "$COMPARE_PGO_LDFLAGS" -o "$NO_PGO_BIN" .
    )

    log "Building PGO binary: $WITH_PGO_BIN"
    (
        cd "$PROJECT_ROOT"
        GOAMD64="$COMPARE_PGO_GOAMD64" \
            "$GO_BIN" build $COMPARE_PGO_BUILD_FLAGS_BASE -pgo=auto \
            -ldflags "$COMPARE_PGO_LDFLAGS" -o "$WITH_PGO_BIN" .
    )
}

run_one() {
    local label=$1
    local run=$2
    local bin=$3
    local db="$WORK_ROOT/${label}-${run}.db"
    local metrics="$WORK_ROOT/${label}-${run}.metrics"
    local log_file="$WORK_ROOT/${label}-${run}.log"
    local cmd

    run_cmd rm -f "$db" "$db-wal" "$db-shm"

    cmd=( /usr/bin/time -f 'real=%e user=%U sys=%S cpu=%P rss=%M' -o "$metrics"
        "$bin" --index-mode
        --path "$COMPARE_PGO_PATH"
        --name compare_pgo
        --db-path "$db"
        --fresh=true
        --verbose
    )
    if [ "$COMPARE_PGO_INCLUDE_HIDDEN" = "true" ]; then
        cmd+=( --include-hidden )
    fi

    run_cmd "${cmd[@]}" >"$log_file" 2>&1

    local real user sys cpu rss
    eval "$(cat "$metrics")"
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$label" "$run" "$real" "$user" "$sys" "$cpu" "$rss"

    run_cmd rm -f "$db" "$db-wal" "$db-shm"
}

main() {
    validate_inputs
    log "Path: $COMPARE_PGO_PATH"
    log "Runs: $COMPARE_PGO_RUNS"
    log "Warmups: $COMPARE_PGO_WARMUPS"
    log "Use sudo: $COMPARE_PGO_USE_SUDO"
    log "Work root: $WORK_ROOT"

    build_binaries

    if [ "$COMPARE_PGO_USE_SUDO" = "true" ]; then
        sudo -v
    fi

    printf 'binary\trun\telapsed_s\tuser_s\tsys_s\tcpu_pct\tmax_rss_kb\n'

    local i
    for ((i = 1; i <= COMPARE_PGO_WARMUPS; i++)); do
        run_one warm_no_pgo "$i" "$NO_PGO_BIN" >/dev/null
        run_one warm_pgo "$i" "$WITH_PGO_BIN" >/dev/null
    done

    local results_file="$WORK_ROOT/results.tsv"
    printf 'binary\trun\telapsed_s\tuser_s\tsys_s\tcpu_pct\tmax_rss_kb\n' >"$results_file"

    for ((i = 1; i <= COMPARE_PGO_RUNS; i++)); do
        run_one no_pgo "$i" "$NO_PGO_BIN" | tee -a "$results_file"
        run_one pgo "$i" "$WITH_PGO_BIN" | tee -a "$results_file"
    done

    printf '\nAverages\n'
    awk -F '\t' '
        NR==1 { next }
        {
            n[$1]++
            elapsed[$1] += $3
            user[$1] += $4
            sys[$1] += $5
            rss[$1] += $7
        }
        END {
            printf "binary\tavg_elapsed_s\tavg_user_s\tavg_sys_s\tavg_max_rss_kb\n"
            for (k in n) {
                printf "%s\t%.3f\t%.3f\t%.3f\t%.0f\n", k, elapsed[k]/n[k], user[k]/n[k], sys[k]/n[k], rss[k]/n[k]
            }
        }
    ' "$results_file"

    log "Detailed logs and metrics kept under $WORK_ROOT"
}

main "$@"
