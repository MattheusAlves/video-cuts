#!/usr/bin/env bash
# Daily cron entry point. Loads .env, runs the pipeline, logs to logs/YYYY-MM-DD.log
set -euo pipefail
cd "$(dirname "$0")"

export PATH="$HOME/.local/bin:$PATH"
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

mkdir -p logs
mkdir -p state
LOG="logs/$(date +%F).log"

{
  echo "=== $(date -Is) yt-short pipeline ==="
  if ! command -v flock >/dev/null 2>&1; then
    echo "ERROR: flock is required to prevent overlapping daily runs." >&2
    exit 1
  fi
  exec 9>state/run-daily.lock
  if ! flock -n 9; then
    echo "ERROR: another daily run is already active." >&2
    exit 1
  fi
  # Avoid starting a long CPU and memory intensive run on an already busy laptop.
  # Values are in KiB; defaults require 2.5 GiB available and at most 75% swap use.
  min_available_kib=${YT_SHORT_MIN_AVAILABLE_KIB:-2621440}
  max_swap_percent=${YT_SHORT_MAX_SWAP_PERCENT:-75}
  if [[ ! $min_available_kib =~ ^[0-9]+$ || ! $max_swap_percent =~ ^[0-9]+$ ]] || (( max_swap_percent > 100 )); then
    echo "ERROR: invalid memory guard configuration" >&2
    exit 1
  fi
  read -r available_kib swap_total_kib swap_free_kib < <(
    awk '/^MemAvailable:/ {a=$2} /^SwapTotal:/ {t=$2} /^SwapFree:/ {f=$2} END {if (a == "" || t == "" || f == "") exit 1; print a, t, f}' /proc/meminfo
  ) || { echo "ERROR: cannot read memory information" >&2; exit 1; }
  swap_percent=0
  if (( swap_total_kib > 0 )); then
    swap_percent=$(( (swap_total_kib - swap_free_kib) * 100 / swap_total_kib ))
  fi
  if (( available_kib < min_available_kib || swap_percent > max_swap_percent )); then
    echo "ERROR: insufficient memory headroom (available: ${available_kib} KiB, swap used: ${swap_percent}%). Close other applications and retry." >&2
    exit 1
  fi
  min_disk_kib=${YT_SHORT_MIN_DISK_KIB:-5242880}
  if [[ ! $min_disk_kib =~ ^[0-9]+$ ]]; then
    echo "ERROR: invalid disk guard configuration" >&2
    exit 1
  fi
  disk_available_kib=$(df -Pk . | awk 'NR == 2 {print $4}')
  if [[ ! $disk_available_kib =~ ^[0-9]+$ ]] || (( disk_available_kib < min_disk_kib )); then
    echo "ERROR: insufficient disk space in the project filesystem; need at least ${min_disk_kib} KiB." >&2
    exit 1
  fi
  go run -buildvcs=false ./cmd/yt-short --pipeline "$@"
  echo "=== $(date -Is) done ==="
} 2>&1 | tee -a "$LOG"
