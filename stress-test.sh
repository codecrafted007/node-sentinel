#!/usr/bin/env bash
#
# stress-test.sh — acceptance test for the contention detector.
#
# It checks the property that matters in production: the agent stays quiet when
# the node is healthy, fires when a pod is genuinely starved of CPU, and goes
# quiet again when the load stops. Run on the Linux host, as root, after build.
#
#   PHASE 1  baseline      -> expect "healthy", no contention
#   PHASE 2  under stress  -> expect "CPU CONTENTION"
#   PHASE 3  recovery      -> expect "healthy" again
#
# Prints PASS/FAIL per phase and exits non-zero if any phase fails, so it can
# gate CI. The agent output for each phase is shown so you can read the numbers.
#
# Usage:
#   sudo ./stress-test.sh [--workers N] [--duration S] [--interval D] [--top N]
#
# Defaults: workers = 4 x nproc, duration 30 (s), interval 5s, top 10.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

AGENT="$ROOT/bin/agent"
WORKERS=$(( $(nproc) * 4 ))
DURATION=30
INTERVAL=5s
TOP=10
UNIT=ns-stress

while [ $# -gt 0 ]; do
  case "$1" in
    --workers)  WORKERS="$2"; shift 2 ;;
    --duration) DURATION="$2"; shift 2 ;;
    --interval) INTERVAL="$2"; shift 2 ;;
    --top)      TOP="$2"; shift 2 ;;
    -h|--help)  sed -n '3,24p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)          echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

log()  { printf '\n\033[1;34m########## %s ##########\033[0m\n' "$*"; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; }

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

run_agent() { timeout "$RUN" "$AGENT" --interval "$INTERVAL" --top "$TOP" 2>&1 || true; }
count()     { printf '%s\n' "$1" | grep -c "$2" || true; }

PASS=1
verdict() { # message  ok(1/0)
  if [ "$2" = 1 ]; then
    printf '  [ \033[1;32mPASS\033[0m ] %s\n' "$1"
  else
    printf '  [ \033[1;31mFAIL\033[0m ] %s\n' "$1"
    PASS=0
  fi
}

# --- PHASE 1: baseline ---
log "PHASE 1 — BASELINE (expect: healthy)"
out=$(run_agent); printf '%s\n' "$out"
healthy=$(count "$out" "healthy")
contention=$(count "$out" "CPU CONTENTION")
[ "$healthy" -ge 1 ] && [ "$contention" -eq 0 ] && ok=1 || ok=0
verdict "stays quiet when healthy (healthy=$healthy, contention=$contention)" "$ok"

# --- PHASE 2: under stress ---
log "PHASE 2 — UNDER STRESS: $WORKERS CPU hogs on $(nproc) cores (expect: contention)"
systemd-run --unit="$UNIT" --collect stress-ng --cpu "$WORKERS" --timeout "${DURATION}s" >/dev/null
sleep 2
out=$(run_agent); printf '%s\n' "$out"
contention=$(count "$out" "CPU CONTENTION")
[ "$contention" -ge 1 ] && ok=1 || ok=0
verdict "detects contention under stress (contention=$contention)" "$ok"

# --- PHASE 3: recovery ---
log "PHASE 3 — RECOVERY (expect: healthy)"
cleanup
sleep 5
out=$(run_agent); printf '%s\n' "$out"
healthy=$(count "$out" "healthy")
[ "$healthy" -ge 1 ] && ok=1 || ok=0
verdict "returns to healthy after load stops (healthy=$healthy)" "$ok"

# --- summary ---
echo
if [ "$PASS" = 1 ]; then
  printf '\033[1;32mRESULT: PASS\033[0m — detector stays quiet when healthy and fires under stress\n'
  exit 0
else
  printf '\033[1;31mRESULT: FAIL\033[0m — a phase did not behave as expected (see above)\n'
  printf 'If baseline flagged contention, the node may be genuinely busy or --runq-warn is too low.\n'
  exit 1
fi
