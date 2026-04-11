#!/bin/bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

INDEXER_BIN="${INDEXER_BIN:-$PROJECT_ROOT/indexer}"
OUTPUT_PGO="${OUTPUT_PGO:-$PROJECT_ROOT/default.pgo}"
WORK_ROOT="${WORK_ROOT:-$(mktemp -d /tmp/indexer-pgo.XXXXXX)}"
KEEP_WORK_ROOT="${KEEP_WORK_ROOT:-false}"
PGO_MODE="${PGO_MODE:-real}"
PGO_PATH="${PGO_PATH:-/}"
PGO_NAME="${PGO_NAME:-pgo_profile}"
PGO_INCLUDE_HIDDEN="${PGO_INCLUDE_HIDDEN:-true}"
PGO_USE_SUDO="${PGO_USE_SUDO:-}"
DATASET_ROOT="${DATASET_ROOT:-$WORK_ROOT/dataset}"
DB_PATH="${DB_PATH:-$WORK_ROOT/indexer.db}"
PROFILE_FRESH="${PROFILE_FRESH:-$WORK_ROOT/fresh.pprof}"
PROFILE_INCREMENTAL="${PROFILE_INCREMENTAL:-$WORK_ROOT/incremental.pprof}"
PROFILE_MERGED="${PROFILE_MERGED:-$WORK_ROOT/merged.pgo}"
PGO_TOP="${PGO_TOP:-20}"
TOP_DIRS="${TOP_DIRS:-12}"
SUB_DIRS="${SUB_DIRS:-24}"
FILES_PER_DIR="${FILES_PER_DIR:-18}"
GO_BIN="${GO_BIN:-$(command -v go || true)}"

if [ -z "$GO_BIN" ]; then
    echo "go binary not found on PATH; set GO_BIN explicitly" >&2
    exit 1
fi

cleanup() {
    if [ "$KEEP_WORK_ROOT" != "true" ] && [ -d "$WORK_ROOT" ]; then
        rm -rf "$WORK_ROOT"
    fi
}
trap cleanup EXIT

log() {
    printf '[pgo] %s\n' "$1"
}

target_path_for_profile() {
    if [ "$PGO_MODE" = "real" ]; then
        printf '%s\n' "$PGO_PATH"
        return
    fi
    printf '%s\n' "$DATASET_ROOT"
}

validate_inputs() {
    case "$PGO_MODE" in
        synthetic|real) ;;
        *)
            echo "unsupported PGO_MODE: $PGO_MODE (expected synthetic or real)" >&2
            exit 1
            ;;
    esac

    if [ "$PGO_MODE" = "real" ]; then
        if [ -z "$PGO_PATH" ]; then
            echo "PGO_PATH is required when PGO_MODE=real" >&2
            exit 1
        fi
        if [ ! -d "$PGO_PATH" ]; then
            echo "PGO_PATH does not exist or is not a directory: $PGO_PATH" >&2
            exit 1
        fi
    fi

    if [ -z "$PGO_USE_SUDO" ]; then
        if [ "$PGO_MODE" = "real" ]; then
            PGO_USE_SUDO=true
        else
            PGO_USE_SUDO=false
        fi
    fi

    case "$PGO_USE_SUDO" in
        true|false) ;;
        *)
            echo "unsupported PGO_USE_SUDO: $PGO_USE_SUDO (expected true or false)" >&2
            exit 1
            ;;
    esac
}

prepare_dataset() {
    if [ "$PGO_MODE" = "real" ]; then
        log "Using real workload path: $PGO_PATH"
        return
    fi

    rm -rf "$DATASET_ROOT"
    mkdir -p "$DATASET_ROOT"

    log "Preparing synthetic dataset under $DATASET_ROOT"
    for top in $(seq 1 "$TOP_DIRS"); do
        top_dir="$DATASET_ROOT/group_$top"
        mkdir -p "$top_dir"
        for sub in $(seq 1 "$SUB_DIRS"); do
            sub_dir="$top_dir/section_$sub"
            mkdir -p "$sub_dir"
            for file in $(seq 1 "$FILES_PER_DIR"); do
                file_path="$sub_dir/file_${file}.txt"
                {
                    printf 'group=%02d section=%02d file=%02d\n' "$top" "$sub" "$file"
                    printf 'payload=%s\n' "$(printf '%064d' "$((top * sub * file))")"
                } > "$file_path"
            done
        done
    done

    mkdir -p "$DATASET_ROOT/.hidden/cache"
    printf 'hidden=true\n' > "$DATASET_ROOT/.hidden/cache/state.txt"
    ln -s "../../group_1/section_1/file_1.txt" "$DATASET_ROOT/group_2/section_1/link_to_file_1.txt"
}

mutate_dataset() {
    if [ "$PGO_MODE" = "real" ]; then
        log "Skipping mutation for real workload mode"
        return
    fi

    log "Mutating dataset for incremental profile"
    mapfile -t files < <(find "$DATASET_ROOT" -type f ! -path '*/.hidden/*' | sort)
    for path in "${files[@]:0:40}"; do
        rm -f "$path"
    done

    target_group="$DATASET_ROOT/group_$TOP_DIRS"
    mkdir -p "$target_group/new_branch"
    for file in $(seq 1 120); do
        printf 'new=%03d\n' "$file" > "$target_group/new_branch/new_${file}.txt"
    done

    for path in "${files[@]:0:60}"; do
        [ -f "$path" ] || continue
        printf 'mutated=%s\n' "$(basename "$path")" >> "$path"
    done
}

build_for_profiling() {
    log "Building indexer without PGO so the profile is unbiased"
    (cd "$PROJECT_ROOT" && make build-only GO_BUILD_FLAGS='-trimpath -buildvcs=false -pgo=off')
}

run_profile() {
    local profile_file=$1
    local fresh_flag=$2
    local target_path
    local cmd
    target_path="$(target_path_for_profile)"

    cmd=( "$INDEXER_BIN" --index-mode \
        --path "$target_path" \
        --name "$PGO_NAME" \
        --db-path "$DB_PATH" \
        --fresh="$fresh_flag" \
        --cpu-profile "$profile_file" \
        --verbose )
    if [ "$PGO_INCLUDE_HIDDEN" = "true" ]; then
        cmd+=( --include-hidden )
    fi

    rm -f "$profile_file"
    log "Capturing CPU profile: $profile_file (path=$target_path fresh=$fresh_flag)"
    if [ "$PGO_USE_SUDO" = "true" ]; then
        sudo "${cmd[@]}"
        return
    fi
    "${cmd[@]}"
}

merge_profiles() {
    log "Merging representative profiles into $OUTPUT_PGO"
    "$GO_BIN" tool pprof -proto "$INDEXER_BIN" "$PROFILE_FRESH" "$PROFILE_INCREMENTAL" > "$PROFILE_MERGED"
    mv "$PROFILE_MERGED" "$OUTPUT_PGO"
}

show_summary() {
    log "Top CPU paths used for PGO input"
    "$GO_BIN" tool pprof -top -nodecount="$PGO_TOP" "$INDEXER_BIN" "$OUTPUT_PGO" || true
}

main() {
    validate_inputs
    log "Working directory: $WORK_ROOT"
    log "Mode: $PGO_MODE"
    log "Use sudo for profiling: $PGO_USE_SUDO"
    build_for_profiling
    prepare_dataset
    run_profile "$PROFILE_FRESH" true
    mutate_dataset
    run_profile "$PROFILE_INCREMENTAL" false
    merge_profiles
    show_summary
    log "Generated $OUTPUT_PGO"
    log "Next step: make build"
}

main "$@"
