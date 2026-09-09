#!/usr/bin/env bash
# addons/benchmark.sh — Run the storage engine benchmark suite and pretty-print results.
#
# Usage:
#   bash addons/benchmark.sh [options] [benchtime]
#
# Options:
#   -o FILE   Save raw go-test output (benchstat-compatible) to FILE
#   -c N      Number of benchmark runs (default: 1; use -c 5 for benchstat)
#
# Examples:
#   bash addons/benchmark.sh                   # default: 3s per benchmark, single pass
#   bash addons/benchmark.sh 5s                # longer run for tighter numbers
#   bash addons/benchmark.sh -c 5 -o new.txt   # 5 runs, save for benchstat comparison
#
# Comparing runs:
#   bash addons/benchmark.sh -o old.txt 3s     # baseline
#   # ... make changes ...
#   bash addons/benchmark.sh -o new.txt 3s     # after changes
#   benchstat old.txt new.txt                  # compare with benchstat

set -euo pipefail

# ── defaults ─────────────────────────────────────────────────────────────────
BENCHTIME=""
RAW_OUTPUT=""
COUNT=1

# ── parse flags ──────────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        -o) RAW_OUTPUT="$2"; shift 2 ;;
        -c) COUNT="$2"; shift 2 ;;
        *)  BENCHTIME="$1"; shift ;;
    esac
done
BENCHTIME="${BENCHTIME:-3s}"

# ── colours ──────────────────────────────────────────────────────────────────
BOLD="\033[1m"
DIM="\033[2m"
CYAN="\033[0;36m"
GREEN="\033[0;32m"
YELLOW="\033[0;33m"
BLUE="\033[0;34m"
MAGENTA="\033[0;35m"
RESET="\033[0m"

# ── helpers ───────────────────────────────────────────────────────────────────
cd "$(dirname "$0")/.."
SUITE_START=$(date +%s)

header() {
    local title="$1"
    local width=70
    local pad=$(( (width - ${#title} - 2) / 2 ))
    local line
    line=$(printf '─%.0s' $(seq 1 $width))
    echo -e "\n${CYAN}┌${line}┐${RESET}"
    printf "${CYAN}│${RESET}%*s${BOLD}${YELLOW} %s ${RESET}%*s${CYAN}│${RESET}\n" \
        "$pad" "" "$title" "$pad" ""
    echo -e "${CYAN}└${line}┘${RESET}\n"
}

subheader() {
    echo -e "${BLUE}${BOLD}  ▸ $1${RESET}"
}

divider() {
    echo -e "${DIM}  $(printf '┄%.0s' $(seq 1 66))${RESET}"
}

# Formats a raw `go test -bench -benchmem` line into an aligned, coloured row.
# Handles the optional MB/s column that appears when SetBytes is used.
format_bench_line() {
    local line="$1"
    [[ "$line" =~ ^Benchmark ]] || return 0

    # Use awk to robustly locate the key fields regardless of column count.
    # go test -benchmem output columns:
    #   Name  Iterations  <val> ns/op  [<val> MB/s]  <val> B/op  <val> allocs/op
    local parsed
    parsed=$(echo "$line" | awk '
    {
        name = $1
        # Strip goroutine suffix (-32, -8, …)
        sub(/-[0-9]+$/, "", name)

        ns_val   = "?"
        b_op     = "?"
        alloc_op = "?"

        for (i = 2; i <= NF; i++) {
            if ($i == "ns/op")   { ns_val   = $(i-1) }
            if ($i == "B/op")    { b_op     = $(i-1) }
            if ($i == "allocs/op") { alloc_op = $(i-1) }
        }
        printf "%s\t%s\t%s\t%s\n", name, ns_val, b_op, alloc_op
    }')

    local name ns_val b_op alloc_op
    IFS=$'\t' read -r name ns_val b_op alloc_op <<< "$parsed"

    # Colour the ns/op value by order of magnitude:
    #   < 1 us   (1 000 ns)    = green   (fast: codec, extraction)
    #   < 100 us (100 000 ns)  = yellow  (moderate: writes, small queries)
    #   >= 100 us              = magenta (slow: large queries, aggregation)
    local ns_color
    local ns_num
    ns_num=$(echo "$ns_val" | tr -d ',' | cut -d. -f1)
    if [[ "$ns_num" =~ ^[0-9]+$ ]] && (( ns_num < 1000 )); then
        ns_color="${GREEN}"
    elif [[ "$ns_num" =~ ^[0-9]+$ ]] && (( ns_num < 100000 )); then
        ns_color="${YELLOW}"
    else
        ns_color="${MAGENTA}"
    fi

    printf "  ${BOLD}%-50s${RESET}  ${ns_color}%12s ns/op${RESET}  ${DIM}%10s B/op  %5s allocs${RESET}\n" \
        "$name" "$ns_val" "$b_op" "$alloc_op"
}

pretty_run() {
    local pkg="$1"
    local pattern="$2"

    local raw
    raw=$(go test \
        -bench="$pattern" \
        -benchmem \
        -benchtime="$BENCHTIME" \
        -count="$COUNT" \
        -run='^$' \
        "$pkg" 2>&1)

    # Append raw output for benchstat if -o was given.
    if [[ -n "$RAW_OUTPUT" ]]; then
        echo "$raw" >> "$RAW_OUTPUT"
    fi

    # Pretty-print only the last iteration of each benchmark (avoid
    # repeating the same line $COUNT times in the terminal).
    local seen=""
    while IFS= read -r line; do
        if [[ "$line" =~ ^Benchmark ]]; then
            local bname
            bname=$(echo "$line" | awk '{ sub(/-[0-9]+$/, "", $1); print $1 }')
            # Track seen names; only print the last occurrence
            seen="${seen}${bname}\n"
        fi
    done <<< "$raw"

    # Second pass: print only the final occurrence of each benchmark name
    local last_seen=""
    while IFS= read -r line; do
        if [[ "$line" =~ ^Benchmark ]]; then
            local bname
            bname=$(echo "$line" | awk '{ sub(/-[0-9]+$/, "", $1); print $1 }')
            if [[ "$last_seen" == *"$bname"* ]]; then
                continue
            fi
            last_seen="${last_seen} ${bname}"
        fi
    done <<< "$raw"

    # Simple approach: deduplicate by printing last line per benchmark name
    declare -A last_line
    local order=()
    while IFS= read -r line; do
        if [[ "$line" =~ ^Benchmark ]]; then
            local bname
            bname=$(echo "$line" | awk '{ sub(/-[0-9]+$/, "", $1); print $1 }')
            if [[ -z "${last_line[$bname]+x}" ]]; then
                order+=("$bname")
            fi
            last_line[$bname]="$line"
        fi
    done <<< "$raw"
    for bname in "${order[@]}"; do
        format_bench_line "${last_line[$bname]}"
    done
}

# ── banner ────────────────────────────────────────────────────────────────────
echo -e "\n${BOLD}${CYAN}  kula — Storage Engine Benchmark Suite${RESET}"
echo -e "${DIM}  benchtime=${BENCHTIME}  count=${COUNT}  pkg=kula/internal/storage${RESET}"
echo -e "${DIM}  $(go version)${RESET}"
if [[ -n "$RAW_OUTPUT" ]]; then
    : > "$RAW_OUTPUT"  # truncate
    echo -e "${DIM}  raw output → ${RAW_OUTPUT}${RESET}"
fi

PKG="kula/internal/storage"

# ═══════════════════════════════════════════════════════════════════
# 1. CODEC
# ═══════════════════════════════════════════════════════════════════
header "Codec — Encode / Decode"

subheader "Sample serialisation (binary encode/decode)"
divider
pretty_run "$PKG" "BenchmarkEncodeSample|BenchmarkDecodeSample"
divider

subheader "Fast timestamp extraction vs full decode"
divider
pretty_run "$PKG" "BenchmarkExtractVsFullDecode|BenchmarkExtractTimestamp"
divider

# ═══════════════════════════════════════════════════════════════════
# 2. WRITE PATH
# ═══════════════════════════════════════════════════════════════════
header "Write Path — Ring Buffer"

subheader "Sequential write throughput"
divider
pretty_run "$PKG" "BenchmarkWrite$"
divider

subheader "Write throughput into a wrapping (small) ring buffer"
divider
pretty_run "$PKG" "BenchmarkWriteWrapping"
divider

subheader "Concurrent writes — lock contention"
divider
pretty_run "$PKG" "BenchmarkWriteParallel"
divider

# ═══════════════════════════════════════════════════════════════════
# 3. READ / QUERY PATH
# ═══════════════════════════════════════════════════════════════════
header "Read Path — QueryRange"

subheader "Small range query (last 60 s of 300 samples)"
divider
pretty_run "$PKG" "BenchmarkQueryRange_Small"
divider

subheader "Repeated large range query (3 600 samples — mostly cache hits)"
divider
pretty_run "$PKG" "BenchmarkQueryRange_Large"
divider

subheader "Query after ring buffer has wrapped multiple times"
divider
pretty_run "$PKG" "BenchmarkQueryRange_Wrapped"
divider

subheader "Uncached history at the 7 500-record source selection target"
divider
pretty_run "$PKG" "BenchmarkQueryRange_SourceBudget"
divider

# ═══════════════════════════════════════════════════════════════════
# 4. QUERYLATEST HOT PATH
# ═══════════════════════════════════════════════════════════════════
header "QueryLatest — In-Memory Cache vs Cold Disk"

subheader "Warm-cache path  (steady state — called every second)"
divider
pretty_run "$PKG" "BenchmarkQueryLatest_Cache"
divider

subheader "Cold-disk path  (once at startup — warmLatestCache)"
divider
pretty_run "$PKG" "BenchmarkQueryLatest_ColdDisk"
divider

# ═══════════════════════════════════════════════════════════════════
# 5. AGGREGATION
# ═══════════════════════════════════════════════════════════════════
header "Aggregation"

subheader "Multi-tier aggregation (60-sample → 1-min window)"
divider
pretty_run "$PKG" "BenchmarkAggregateSamples"
divider

subheader "Policy reducer only — raw and cascading envelopes"
divider
pretty_run "$PKG" "^BenchmarkAggregation$"
divider

subheader "Downsampling 3 600 samples — separate cold queries and cache hits"
divider
pretty_run "$PKG" "BenchmarkDownsampling"
divider

# ═══════════════════════════════════════════════════════════════════
# DONE
# ═══════════════════════════════════════════════════════════════════
SUITE_END=$(date +%s)
ELAPSED=$(( SUITE_END - SUITE_START ))
MINS=$(( ELAPSED / 60 ))
SECS=$(( ELAPSED % 60 ))

echo -e "\n  Benchmark run ${GREEN}${BOLD}complete${RESET}  (benchtime=${BENCHTIME}  count=${COUNT}  elapsed=${MINS}m${SECS}s)"
if [[ -n "$RAW_OUTPUT" ]]; then
    echo -e "  ${DIM}Raw output saved to ${RAW_OUTPUT} — compare with: benchstat old.txt ${RAW_OUTPUT}${RESET}"
fi
echo ""
