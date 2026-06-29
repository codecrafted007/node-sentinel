#!/usr/bin/env bash
# A redacted, representative walkthrough of node-sentinel for the demo GIF
# (rendered by demo.tape via `vhs`). No real cluster is contacted — the output
# mirrors a real run with generic pod/node names, so nothing cluster-specific
# leaks. Pacing comes from the sleeps.
P='\033[36m❯\033[0m '
G='\033[32m'; R='\033[31m'; Y='\033[33m'; D='\033[2m'; N='\033[0m'; B='\033[1m'
step(){ printf '%b%b\n' "$P" "$1"; sleep 0.7; }
out(){ printf '%b\n' "$1"; }

clear
printf '%b\n\n' "${B}node-sentinel${N} ${D}— eBPF noisy-neighbour detection & remediation for Kubernetes${N}"
sleep 1.2

step "kubectl apply -f deploy/"
out "${D}namespace/sentinel-system created${N}"
out "${D}daemonset.apps/node-sentinel-agent created     ${N}# one host agent per node (eBPF)"
out "${D}deployment.apps/node-sentinel-controller created${N}"
sleep 1.1

step "kubectl -n sentinel-system logs ds/node-sentinel-agent"
out "sched observer attached     ${D}# run-queue latency + on-CPU time${N}"
out "blkio observer attached     ${D}# block-I/O latency${N}"
out "net observer attached       ${D}# TCP retransmits${N}"
out "pod resolver: 148 containers mapped   ${D}# cgroup → pod, across the whole node${N}"
out "${G}[OK] healthy — no contention (148 cgroups seen)${N}"
sleep 1.3

printf '\n%b\n' "${D}# a batch job starts hammering the CPU…${N}"
step "kubectl apply -f noisy-neighbour.yaml"
out "${D}pod/noisy-demo created${N}"
sleep 1.3

step "kubectl -n sentinel-system logs ds/node-sentinel-agent   ${D}# the agent's verdict${N}"
out "${R}[!] CONTENTION — CPU: 14 victim(s)${N}"
out "  ── CPU ──  ${G}attribution: confident pod offender (100% >= 70%)${N}"
out "  ${B}OFFENDERS — by CPU time${N}"
out "  POD                          CPU_MS  INTENSITY  THROTTLE  CORREL  CONFIDENCE  VERDICT"
out "  ${R}sentinel-system/noisy-demo    18184    91.0%       —       33%      100%${N}   OVER fair share"
out "  payments/checkout-api           173     0.9%       —       60%        0%   within request"
out "  search/indexer                   59     0.3%       —       50%        1%   within request"
out "  ${D}  …14 more, all 0% — busy ≠ guilty (high usage is their own normal)${N}"
out "  ${B}VICTIMS — by run-queue latency${N}"
out "  POD                           P50_US     P99_US   xBASELINE   EVENTS"
out "  team-a/orders-api            196608    3145728     18783x      147   ${D}# starved ~3.1s${N}"
out "  team-b/web-frontend          393216    3145728     20856x      136"
out "  search/query-api              49152    1572864      7737x      134"
sleep 1.6

printf '\n%b\n' "${D}# NodeHealthPolicy mode=enforce → the controller acts (timeout, not eviction):${N}"
step "kubectl get events --field-selector reason=NoisyNeighborThrottled"
out "${Y}Warning  NoisyNeighborThrottled  pod/noisy-demo${N}"
out "  throttled noisy-demo CPU limit ${B}3 → 100m${N}  ${D}(in-place /resize; kubelet actuates)${N}"
sleep 1.1
out "  ${D}…2 min later…${N}"
out "  ${G}restored${N} noisy-demo CPU limit to 3  ${D}(throttle window elapsed)${N}"
sleep 1.2

printf '\n%b\n' "${G}✓${N} detect → attribute ${D}(with confidence)${N} → throttle → restore"
sleep 2.2
