#!/usr/bin/env bash
#
# overhead.sh — measure the agent's overhead against the budget (design §16):
#   * agent userspace CPU  < 1% of the node
#   * agent RSS            < 50 MB
#   * BPF handlers          ~100-200 ns per event
#
# Measures idle and under stress (overhead scales with context-switch rate, not
# cluster size). Run on the Linux host, as root, after ./scripts/build.sh.
#
# Usage: sudo ./scripts/overhead.sh [--window S] [--stress-workers N]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

AGENT="$ROOT/bin/agent"
WINDOW=15
STRESS_WORKERS=$(( $(nproc) * 2 ))
NCPU=$(nproc)
UNIT_AGENT=ns-agent-oh
UNIT_STRESS=ns-stress-oh

while [ $# -gt 0 ]; do
  case "$1" in
    --window)         WINDOW="$2"; shift 2 ;;
    --stress-workers) STRESS_WORKERS="$2"; shift 2 ;;
    -h|--help)        sed -n '3,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)                echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

err() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; }
log() { printf '\n\033[1;34m== %s ==\033[0m\n' "$*"; }

[ "$(uname -s)" = "Linux" ] || { err "Linux host only."; exit 1; }
[ "$(id -u)" = "0" ]       || { err "run as root (sudo)."; exit 1; }
[ -x "$AGENT" ]           || { err "missing $AGENT — run 'make build' first."; exit 1; }

cleanup() {
  systemctl stop "$UNIT_AGENT" "$UNIT_STRESS" 2>/dev/null || true
  systemctl reset-failed "$UNIT_AGENT" "$UNIT_STRESS" 2>/dev/null || true
}
trap cleanup EXIT
cleanup

# Enable BPF runtime stats so bpftool can report per-program run time.
prev_stats=$(cat /proc/sys/kernel/bpf_stats_enabled 2>/dev/null || echo 0)
sysctl -wq kernel.bpf_stats_enabled=1 2>/dev/null || true
restore_stats() { sysctl -wq kernel.bpf_stats_enabled="$prev_stats" 2>/dev/null || true; }
trap 'restore_stats; cleanup' EXIT

log "starting agent"
systemd-run --unit="$UNIT_AGENT" --collect "$AGENT" --interval 5s --metrics-addr "" --local-socket "" >/dev/null
sleep 5
PID=$(pgrep -f "$AGENT" | head -1)
[ -n "$PID" ] || { err "agent did not start"; exit 1; }
CLK=$(getconf CLK_TCK)

# measure_cpu LABEL: sample agent utime+stime over WINDOW, report CPU% and RSS.
measure_cpu() {
  read -r u1 s1 < <(awk '{print $14, $15}' "/proc/$PID/stat")
  sleep "$WINDOW"
  read -r u2 s2 < <(awk '{print $14, $15}' "/proc/$PID/stat")
  local rss
  rss=$(awk '/VmRSS/{print $2}' "/proc/$PID/status")
  awk -v t="$(( (u2+s2) - (u1+s1) ))" -v clk="$CLK" -v w="$WINDOW" -v n="$NCPU" -v rss="$rss" -v lbl="$1" 'BEGIN{
    cpu=t/clk; core=cpu/w*100; node=core/n; mb=rss/1024;
    printf "%-12s agent CPU: %.2f%% of one core  |  %.3f%% of %d-core node  |  RSS: %.1f MB\n", lbl, core, node, n, mb;
  }'
}

log "measuring idle (${WINDOW}s)"
measure_cpu "idle:"

log "measuring under stress: $STRESS_WORKERS CPU workers (${WINDOW}s)"
systemd-run --unit="$UNIT_STRESS" --collect stress-ng --cpu "$STRESS_WORKERS" --timeout "$((WINDOW + 8))s" >/dev/null 2>&1
sleep 3
measure_cpu "under-stress:"
systemctl stop "$UNIT_STRESS" 2>/dev/null || true

log "BPF handler cost (avg ns/event, needs a few seconds of events)"
bpftool prog show 2>/dev/null | awk '
  /handle_sched_/ { name=$0; sub(/.*name /,"",name); sub(/ .*/,"",name) }
  /run_time_ns/ {
    for (i=1;i<=NF;i++){ if($i=="run_time_ns") rt=$(i+1); if($i=="run_cnt") rc=$(i+1) }
    if (name!="" && rc>0) printf "  %-22s %6.0f ns/event over %s events\n", name, rt/rc, rc
    name=""
  }' || echo "  (bpftool stats unavailable)"

log "BPF map kernel memory"
bpftool map show 2>/dev/null | awk '
  /(runq_latency|wakeup_ts|cpu_time|cpu_slice)/ {
    name=$0; sub(/.*name /,"",name); sub(/ .*/,"",name)
    for (i=1;i<=NF;i++) if($i=="bytes_memlock") b=$(i+1)
    total+=b; printf "  %-22s %8.2f MB\n", name, b/1048576
  }
  END { if (total) printf "  %-22s %8.2f MB\n", "TOTAL", total/1048576 }' || echo "  (bpftool unavailable)"

log "budget: agent < 1% of node CPU, RSS < 50 MB, BPF handlers ~100-200 ns"
