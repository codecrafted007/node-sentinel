#!/usr/bin/env bash
#
# stress-test.sh — validate the agent by injecting CPU contention and watching
# run-queue latency rise, then recover. Run on the Linux build host, as root,
# after the agent is built (./build.sh).
#
# Flow:  baseline (idle)  ->  inject stress-ng CPU hogs  ->  measure  ->  stop  ->  recovery
#
# What to expect: run-queue p50 climbs from a few µs to hundreds of µs under load
# (p99 into the multi-ms range) across the resolved pods, then snaps back within
# seconds of stopping the load. The hog cgroup itself barely shows up — run-queue
# latency is a victim-side signal (CPU hogs rarely sleep, so they emit few
# wakeup->switch events). Identifying the offender needs CPU-time intensity
# (design §7.5 step 2), not yet built.
#
# Usage:
#   sudo ./stress-test.sh [--workers N] [--duration S] [--interval D] [--top N]
#
# Defaults: workers = 4 x nproc, duration 45 (s), interval 5s, top 10.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

AGENT="$ROOT/bin/agent"
WORKERS=$(( $(nproc) * 4 ))
DURATION=45
INTERVAL=5s
TOP=10
UNIT=ns-stress

while [ $# -gt 0 ]; do
  case "$1" in
    --workers)  WORKERS="$2"; shift 2 ;;
    --duration) DURATION="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --top)      TOP="$2"; shift 2 ;;
    -h|--help)  sed -n '3,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)          echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\n\033[1;34m########## %s ##########\033[0m\n' "$*"; }
err() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; }

# --- preflight ---
[ "$(uname -s)" = "Linux" ] || { err "Linux host only (eBPF)."; exit 1; }
[ "$(id -u)" = "0" ]        || { err "run as root (sudo) — the agent needs BPF capabilities."; exit 1; }
[ -x "$AGENT" ]            || { err "missing $AGENT — run ./build.sh first."; exit 1; }
command -v stress-ng >/dev/null 2>&1 || { err "stress-ng not found — install it: sudo apt-get install -y stress-ng"; exit 1; }
command -v systemd-run >/dev/null 2>&1 || { err "systemd-run not found (needs systemd)."; exit 1; }

# Run the agent long enough to print ~2 intervals (interval is like "5s").
ISECS="${INTERVAL%s}"
RUN=$(( ISECS * 2 + 3 ))

cleanup() { systemctl stop "$UNIT" 2>/dev/null || true; systemctl reset-failed "$UNIT" 2>/dev/null || true; }
trap cleanup EXIT
cleanup  # clear any leftover unit from a previous run

log "BASELINE (idle, ~${RUN}s)"
timeout "$RUN" "$AGENT" --interval "$INTERVAL" --top "$TOP" || true

log "INJECT CONTENTION: $WORKERS CPU hogs on $(nproc) cores for ${DURATION}s"
systemd-run --unit="$UNIT" --collect stress-ng --cpu "$WORKERS" --timeout "${DURATION}s" >/dev/null
sleep 2

log "DURING contention (~${RUN}s)"
timeout "$RUN" "$AGENT" --interval "$INTERVAL" --top "$TOP" || true

log "STOP contention + let the run queue settle"
cleanup
sleep 4

log "RECOVERY (~${RUN}s)"
timeout "$RUN" "$AGENT" --interval "$INTERVAL" --top "$TOP" || true

log "done — compare p50/p99: a few µs idle -> hundreds of µs / multi-ms under load -> back to baseline"
