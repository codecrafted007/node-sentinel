# node-sentinel — Engineering Internals

## Program Flow & Scale Reference

**Status:** v0.2
**Author:** Brajesh Pant
**Date:** April 2026
**Companion to:** node-sentinel Design Document v0.3

---

## How To Read This Document

If you are new to the project:

1. Read **Section 1** (end-to-end flow) — trace one contention event through the entire system
2. Read **Section 3** (agent pipeline) — understand how kernel data becomes pod metrics
3. Read **Section 5** (controller evaluation loop) — understand how decisions are made

Then come back to the kernel details (Section 2) and scale analysis (Sections 8-9) when you need depth.

---

## System Mental Model

Before diving into details, anchor this in your head:

```
eBPF (kernel)    → collects signals      (what's happening at the hardware level)
Agent (Go)       → aggregates + detects  (which pod is anomalous)
Controller (Go)  → decides + acts        (what to do about it)
Kubernetes       → enforces actions      (taint, cordon, evict)
```

Every section below maps to one of these four stages. Data flows left to right. Each stage is strictly forward-only — no feedback into earlier stages. The controller never tells a BPF program what to look for, and the agent never asks the controller what to report. This keeps the system predictable and easy to debug.

---

## System Guarantees

Four properties that hold under all conditions:

1. **Detects sustained contention, not transient spikes.** Burst filtering and evaluation windows ensure the system only acts on persistent problems. A 10-second I/O spike is ignored. A 2-minute I/O flood triggers action.
2. **Attribution is probabilistic, not deterministic.** The system identifies correlation, not causation. Confidence scores quantify how certain the attribution is. When uncertain, the system alerts but does not act.
3. **Actions are always safety-gated and reversible.** Every automated action (taint, cordon, evict) passes through confidence gates, cooldowns, rate limits, PDB checks, and workload type checks. Every action can be undone — manually or automatically on recovery.
4. **The system prefers false negatives over false positives.** Missing a noisy neighbor is less harmful than wrongly blaming a pod and evicting it. The entire confidence and safety model is calibrated around this principle.

**One thing to internalize early:** Confidence is a gating mechanism, not a signal — actions are driven by *both* severity and confidence. High severity alone doesn't trigger action (confidence might be too low). High confidence alone doesn't trigger action (severity might be Healthy). Both must exceed their respective thresholds for any remediation to occur.

---

## Purpose

The design document describes *what* node-sentinel does and *why*. This document describes *how data moves through the system* and *how far it can scale* — with real numbers, worked examples, and benchmark methodology to validate every claim.

If you're joining this project, read this after the design doc. It will give you a concrete mental model of what happens from the moment a kernel event fires to the moment a node gets tainted.

---

## Table of Contents

1. [End-to-End Data Flow — Traced With Real Numbers](#1-end-to-end-data-flow)
2. [Stage 1: Kernel — eBPF Program Execution](#2-stage-1-kernel)
3. [Stage 2: Agent — Map Reading and Aggregation](#3-stage-2-agent)
4. [Stage 3: Agent → Controller — gRPC Transport](#4-stage-3-grpc-transport)
5. [Stage 4: Controller — Evaluation and Decision](#5-stage-4-controller)
6. [Stage 5: Controller → Kubernetes — Action Execution](#6-stage-5-action)
7. [Stage 6: Recovery Loop](#7-stage-6-recovery)
8. [Scale Analysis — Per Component](#8-scale-analysis)
9. [Scale Tiers — What Works, What Breaks, What to Adapt](#9-scale-tiers)
10. [Benchmark Methodology](#10-benchmark-methodology)

---

## 1. End-to-End Data Flow — Traced With Real Numbers

Let's trace a single contention event from start to finish. A Spark executor pod starts writing 500MB/s to disk on worker-03. Here's what happens at every stage, with timing.

```
T+0.000s   Kernel: block_rq_insert fires for a 4KB write from Spark pod
           └── BPF handler: ~150ns execution time
               ├── records timestamp + cgroup_id in inflight_rq map
               └── returns (kernel continues processing the block request)

T+0.002s   Kernel: block_rq_complete fires for the same request
           └── BPF handler: ~200ns execution time
               ├── looks up inflight_rq → found, delta = 2ms
               ├── updates blkio_latency_map[cgroup_id].slots[log2(2ms)]++
               ├── delta < kernel threshold (50ms) → no ring buffer push
               └── deletes inflight_rq entry

           ... this repeats ~125,000 times per second at 500MB/s with 4KB writes ...

T+5.000s   Agent: map reader goroutine wakes up (5-second timer)
           └── reads blkio_latency_map
               ├── iterates ~100 cgroup entries (100 pods on this node)
               ├── for each entry: merges per-CPU values (sum 32 CPU copies)
               ├── resolves cgroup_id → "data-pipeline/spark-executor-7f8b4"
               ├── computes from histogram: p50=1.5ms, p95=8ms, p99=45ms
               ├── compares to rolling baseline: baseline p99=3ms → 15x degradation
               └── checks victim pods: web-frontend p99 went from 2ms to 180ms
           └── builds ContentionReport protobuf (~2KB)
           └── sends via gRPC stream to controller
           Total time: ~15ms for map read + aggregation

T+5.050s   Controller: gRPC server receives ContentionReport from worker-03
           └── stores in sliding window buffer for worker-03

T+10.00s   Controller: evaluation cycle fires (10-second timer)
           └── for worker-03:
               ├── collects reports from last 2 minutes → only 1 report so far
               ├── minSamples=6 → NOT MET → skip
               └── no action yet (need more data)

           ... 50 more seconds pass, 5 more reports arrive ...

T+60.00s   Controller: evaluation cycle
           └── for worker-03:
               ├── 6 reports in window → minSamples met
               ├── aggregate IO latency p99 across reports: 165ms
               ├── 165ms > 100ms warning threshold → YES
               ├── 165ms > 300ms critical threshold → NO
               ├── severity: WARNING
               ├── attribution: spark-executor intensity=0.91, fair_share=0.10
               ├── excess=0.81, temporal correlation with victims=0.87
               ├── confidence=0.83
               ├── mode=enforce, confidence 0.83 >= taint threshold 0.7 → permitted
               └── but severity is only WARNING → action = Event + Metric only
           └── emits K8s event: "NoisyNeighborDetected (WARNING)"
           └── annotates node: sentinel.io/contention-type=io

           ... Spark executor ramps up, I/O latency worsens ...

T+180.0s   Controller: evaluation cycle
           └── for worker-03:
               ├── aggregate IO latency p99: 340ms
               ├── 340ms > 300ms critical threshold → YES
               ├── severity: CRITICAL (sustained for full 2m window)
               ├── offender: spark-executor, confidence=0.88
               ├── confidence 0.88 >= taint threshold 0.7 → taint permitted
               ├── confidence 0.88 < evict threshold 0.9 → eviction NOT permitted
               └── cluster safety: 0 nodes currently tainted < 10% limit → OK
           └── patches node: add taint sentinel.io/noisy-neighbor:NoSchedule
           └── patches node: annotations updated
           └── creates NodeContentionStatus CR
           └── emits event: "NoisyNeighborDetected (CRITICAL), taint applied"
           Total evaluation time for this node: ~3ms
```

**End-to-end latency breakdown:**

| Stage | Latency | Bottleneck |
|-------|---------|------------|
| Kernel event → BPF map update | <1μs | None — inline with kernel event |
| BPF map accumulation | 5s (agent poll interval) | Configurable — this is intentional batching |
| Map read + aggregation | ~15ms | Map iteration syscalls |
| Agent → Controller gRPC | ~50ms | Network RTT |
| Controller evaluation | ~3ms per node | Attribution math |
| K8s API call (taint) | ~100ms | API server RTT |
| **Total: kernel event → taint applied** | **~5-15s** (dominated by poll interval + evaluation interval) | |

The system is not real-time. It's designed around 5-15 second detection-to-action latency. Batching over 5-second windows is intentional — it reduces noise, improves statistical stability of percentiles, and avoids overreacting to short-lived spikes. A single 50ms run queue latency event means nothing. A sustained p99 of 50ms over thousands of events in a 5-second window is a real signal. For a system that applies taints (not kills processes), this latency is appropriate.

**End-to-end pipeline recap:**

```
Kernel:
  events → histograms (per cgroup, per CPU, in-kernel)

Agent:
  histograms → merge per-CPU → percentiles → baseline comparison → anomaly detection

Controller:
  anomalies → attribution (who caused it?) → confidence scoring → action decision

Kubernetes:
  taint / cordon / evict → enforce isolation → recovery when contention resolves
```

---

## 2. Stage 1: Kernel — eBPF Program Execution

### What runs inside the kernel

Four BPF programs attach to kernel tracepoints. Each one fires synchronously with the kernel event — there's no queue, no delay, no sampling. Every event is processed.

**sched_monitor** (CPU contention):
```
Hook: tp/sched/sched_wakeup
  → Records wakeup timestamp for the task's pid
  → Map write: wakeup_ts_map[pid] = bpf_ktime_get_ns()
  → Cost: ~100ns

Hook: tp/sched/sched_switch
  → Looks up wakeup timestamp for the incoming task
  → Computes delta = now - wakeup_time (this IS the run queue latency)
  → Looks up cgroup_id via bpf_get_current_cgroup_id()
  → Increments histogram bucket: runq_latency_map[cgroup_id].slots[log2(delta)]++
  → If delta > threshold: pushes event to ring buffer
  → Cleans up wakeup_ts_map
  → Cost: ~200ns
```

**blkio_monitor** (I/O contention):
```
Hook: tp/block/block_rq_insert
  → Records start time, cgroup_id, bytes, op type for this request
  → Map write: inflight_rq[request_ptr] = {now, cgroup_id, bytes, op}
  → Cost: ~150ns

Hook: tp/block/block_rq_complete
  → Looks up start time from inflight_rq
  → Computes delta (I/O latency)
  → Updates histogram + byte counters in blkio_latency_map
  → Cleans up inflight_rq
  → Cost: ~200ns
```

**net_monitor** (network contention):
```
Hook: kprobe/tcp_retransmit_skb
  → Reads cgroup_id from socket
  → Increments: net_stats_map[cgroup_id].retransmits++
  → Cost: ~150ns

Hook: tp/skb/kfree_skb
  → Reads cgroup_id and drop reason
  → Increments: net_stats_map[cgroup_id].drops++
  → Cost: ~100ns
```

### Why per-CPU maps matter

`sched_switch` fires 10K-50K times per second per CPU core. On a 32-core node, that's up to 1.6M events per second. If all CPUs wrote to the same hash map, they'd serialize on a spinlock.

Per-CPU maps give each core its own copy. Zero lock contention. The cost is memory (32 copies of the map) and merge work (the agent sums 32 copies when reading). This trade-off is overwhelmingly worth it at high event rates.

### The ring buffer — urgent events

Most data flows through the hash maps on a 5-second timer. But if a single event is extreme (say, 500ms run queue latency), we want the agent to know immediately. The BPF program pushes these to a ring buffer:

```c
if (delta > KERNEL_SIDE_THRESHOLD) {
    struct sentinel_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (e) {
        e->timestamp_ns = bpf_ktime_get_ns();
        e->cgroup_id = cgroup_id;
        e->value = delta;
        e->event_type = SCHED_LATENCY;
        bpf_ringbuf_submit(e, 0);
    }
}
```

The Go agent has a goroutine blocking on `ringbuf.Reader.Read()` (epoll-based). It wakes up within microseconds of the push. These urgent events get forwarded to the controller immediately and also update Prometheus metrics.

**Important: the hash maps are the source of truth, not the ring buffer.** The maps capture every event and aggregate them into histograms. The ring buffer is a performance optimization — it lets the agent learn about extreme events immediately instead of waiting for the next 5-second map read. If the ring buffer overflows, drops events, or is temporarily unavailable, the system continues working correctly from the maps alone. Never treat a missing ring buffer event as data loss.

---

## 3. Stage 2: Agent — Map Reading and Aggregation

### The map reader loop

Every 5 seconds (configurable), the agent reads all BPF maps:

```
For each observer (sched, blkio, net, mm):
  1. ITERATE: Walk the per-CPU hash map using cilium/ebpf's MapIterator
     → Each iteration returns: key (cgroup_id) + value (per-CPU array of histograms)
  
  2. MERGE: For each key, sum the per-CPU values
     → 32-core node means summing 32 histogram copies
     → histogram.slots[i] = sum(cpu_0.slots[i] + cpu_1.slots[i] + ... + cpu_31.slots[i])
     → histogram.total_ns = sum(all CPUs)
     → histogram.count = sum(all CPUs)
  
  3. RESOLVE: Map cgroup_id → pod identity
     → Lookup in-memory cache: cgroup_id → {namespace, pod, container, ownerKind}
     → Cache miss: log warning, label as "unknown", skip for attribution
  
  4. PERCENTILE: Compute p50, p95, p99 from the log2 histogram
     → Walk buckets from low to high, accumulate counts
     → p99 = bucket where cumulative_count >= 0.99 * total_count
     → The value is the midpoint of that bucket's range: 2^bucket_index * 1.5
     → This gives ~2x precision (log2 buckets), which is sufficient for threshold comparison
     → Why log2 buckets: 26 buckets cover the entire range from <1ns to >33s using
        only 26 × 8 bytes = 208 bytes of memory per cgroup. A nanosecond-precision
        histogram would need millions of buckets. Log2 gives constant memory with
        acceptable precision for percentile estimation — we need to know "p99 is ~50ms"
        not "p99 is exactly 47.3ms." The thresholds we compare against are coarse (20ms,
        50ms, 100ms), so ~2x precision is more than sufficient.
  
  5. BASELINE: Compare against rolling baseline (exponential moving average)
     → baseline_new = alpha * current_p99 + (1 - alpha) * baseline_old
     → deviation = current_p99 / baseline_p99
     → If deviation > configured sigma threshold → flag as anomalous
  
  6. DELETE: Remove all keys from the map (reset for next interval)
     → For each key: map.Delete(key)
     → Race condition: a BPF program might update a key between our Read and Delete
     → Impact: one interval's worth of data for that cgroup is lost
     → This is acceptable because we treat maps as interval-based snapshots,
        not exact accounting. The system never assumes it captured every single
        kernel event — it captured enough to compute statistically meaningful
        percentiles over 5-second windows. A few lost events at interval
        boundaries do not affect p99 accuracy at these sample sizes.
     → The lost data naturally appears in the next interval's snapshot
```

### Cgroup resolution — the glue layer

BPF gives us a `cgroup_id` (a kernel inode number). We need `namespace/pod/container`. The resolver maintains an in-memory map built at startup and kept fresh:

```
Startup:
  Walk /sys/fs/cgroup/kubepods.slice/ recursively
  For each leaf cgroup directory:
    stat() the directory → inode number = cgroup_id
    Parse path: .../kubepods-burstable-pod<UID>.slice/cri-containerd-<CID>.scope
    Extract pod UID and container ID from path segments
    Call CRI gRPC API: ContainerStatus(CID) → get namespace, pod name, labels
    Store: map[cgroup_id] = {namespace, pod, container, ownerKind, qosClass}

Ongoing:
  inotify watch on /sys/fs/cgroup/kubepods.slice/
  On CREATE: resolve new cgroup, add to map
  On DELETE: remove from map
  Every 60s: full rescan as safety net (catches missed inotify events)
```

**Runtime variations the resolver handles:**
- containerd: `cri-containerd-<64-hex-chars>.scope`
- CRI-O: `crio-<64-hex-chars>.scope`
- Nested systemd slices: variable depth, resolver walks bottom-up
- Sandbox (pause) containers: resolved but excluded from offender attribution

**If cgroup resolution fails** (CRI socket unreachable, path doesn't match known patterns, race with container deletion), the entry is labeled as `unknown` and excluded from attribution entirely. We never attribute contention to an unidentified cgroup — doing so would risk false positives against system processes or transient containers. The `sentinel_agent_cgroup_resolution_errors_total` metric tracks these failures.

### Building the ContentionReport

After processing all observers, the agent builds a protobuf message:

```protobuf
message ContentionReport {
  string node_name = 1;
  int64 timestamp_monotonic_ns = 2;
  int64 timestamp_wall_ns = 3;
  repeated PodMetrics pod_metrics = 4;    // top-K pods by intensity
  NodeAggregates node_aggregates = 5;     // node-wide p50/p95/p99
  repeated UrgentEvent urgent_events = 6; // from ring buffer since last report
}

message PodMetrics {
  string namespace = 1;
  string pod = 2;
  string container = 3;
  string owner_kind = 4;
  MetricSet cpu = 5;
  MetricSet blkio = 6;
  MetricSet network = 7;
  float intensity = 8;         // this pod's share of total resource usage
  float baseline_deviation = 9; // current / baseline ratio
  bool anomalous = 10;
}

message MetricSet {
  float p50 = 1;
  float p95 = 2;
  float p99 = 3;
  uint64 total_count = 4;
  uint64 total_value = 5;
}
```

The report includes the **top 50 pods by intensity** (not all pods). Top-K filtering ensures bounded report size regardless of pod density, while still capturing dominant offenders — in practice, the top 5-10 pods by intensity account for the vast majority of contention. Well-behaved pods with low intensity and no anomaly flag are omitted. Node-level aggregates are always included.

---

## 4. Stage 3: Agent → Controller — gRPC Transport

### Connection model

Each agent maintains a single persistent bidirectional gRPC stream to the controller:

```
Agent                                          Controller
  │                                               │
  ├──── ReportStream (bidirectional) ─────────────┤
  │     Agent sends: ContentionReport (every 10s) │
  │     Controller sends: AgentDirective (rare)   │
  │                                               │
```

The stream stays open indefinitely. If it breaks:
- Agent detects via keepalive failure (30s probe interval)
- Agent reconnects with exponential backoff: 1s, 2s, 4s, 8s, max 60s
- During disconnection: agent buffers up to 10 reports in memory (ring buffer)
- On reconnect: buffered reports are sent immediately, then normal flow resumes
- Reports older than 2× evaluation window (default: 4 minutes) are dropped on the agent side — stale data is worse than no data

### Message sizes

| Cluster profile | Pods/node | Report size | Reports/sec (100 nodes) |
|----------------|-----------|-------------|------------------------|
| Typical | 50 | ~1.2KB | 10/s |
| Dense | 250 | ~3KB | 10/s |
| Very dense | 500 | ~5KB (top 50 pods) | 10/s |
| Extreme (1K+) | 1000+ | ~5KB (still top 50) | 10/s |

Report size is bounded by top-K filtering. Even at 1000+ pods per node, we only send the top 50 pods in each report.

---

## 5. Stage 4: Controller — Evaluation and Decision

### The evaluation loop — what happens every 10 seconds

```
For each node with reports in the last 2 minutes:

  1. POLICY LOOKUP
     → Find NodeHealthPolicy matching this node's labels
     → If multiple match, pick highest priority
     → Cache the resolved policy (re-resolve on CRD change)

  2. WINDOW ASSEMBLY
     → Collect all ContentionReports for this node within evaluation.window (2m)
     → Count reports: if < minSamples (6), skip this node entirely
     → This prevents acting on startup, agent restarts, or data gaps

  3. AGGREGATION
     → For each metric (cpu_p99, io_p99, net_retransmit_rate):
        Compute the metric across all reports in the window
        Method: take the median of the per-report p99 values
        (median of p99s is more stable than a single p99 reading)

  4. SEVERITY CLASSIFICATION
     → Compare aggregated metrics against thresholds:
        All below warning → Healthy
        Any above warning, none above critical → Warning
        Any above critical, none above emergency → Critical
        Any above emergency → Emergency

  5. ATTRIBUTION (only if severity >= Warning)
     → For each pod in the reports:
        a. Compute intensity = pod's resource usage / total node usage
        b. Compute fair_share = pod's resource request / total requests
        c. excess = intensity - fair_share
        d. If excess > 0: candidate offender
     → For each candidate:
        a. Compute temporal correlation with max victim degradation
           across report intervals in the window
        b. Score = excess × correlation
     → Rank by score, take top 3
     → Compute confidence per offender (see design doc Section 7.5)
     → Check for diffuse contention: if top-3 combined excess < 60% of total
        → cap confidence at 0.5

  6. CONFIDENCE GATE
     → For each potential action at this severity level:
        Taint: requires confidence >= 0.7
        Cordon: requires confidence >= 0.8
        Evict: requires confidence >= 0.9
     → Strip actions where confidence is insufficient

  7. MODE CHECK
     → observe: stop here, emit metric only
     → alert: emit event + metric, no remediation
     → enforce: proceed

  8. DEBOUNCE
     → Escalation (severity going up): act immediately
     → De-escalation (severity going down): require 3 consecutive lower readings
     → Same severity as before: maintain current state

  9. COOLDOWN + RATE LIMIT
     → If this node was acted on within cooldownAfterAction (5m): skip
     → If maxActionsPerHour (5) for this node is exhausted: skip
     → If cluster-wide limits hit: defer (queue, don't drop)

  10. EVICTION SAFETY (only if eviction is in the action set)
      → Run 7-step safety checklist (design doc Section 7.6)
      → If any check fails: replace eviction with fallback action (cordon)

  11. EXECUTE
      → API calls: taint node, cordon node, evict pod, create event
      → Update node annotations (sentinel.io/*)
      → Update NodeContentionStatus CR

  Total time per node: 2-5ms (dominated by attribution math)
  Total time for 100 nodes: 200-500ms of each 10s cycle
```

### Attribution — worked example with numbers

Two key terms before we walk through the math:

- **intensity** = a pod's actual share of total resource usage on the node (0.0 to 1.0). If a pod consumed 780ms of CPU time out of 1000ms total across all pods, its intensity is 0.78.
- **fair_share** = the share a pod *should* get based on its resource requests. If a pod requests 250m CPU and total requests across all pods is 1000m, its fair share is 0.25.

When intensity >> fair_share, the pod is consuming more than its fair share — it's a candidate offender.

Node has 5 pods. CPU run queue latency p99 is 65ms (critical threshold: 50ms).

```
Pod                    CPU Time  Fair Share  Intensity  Excess
spark-executor         780ms/s   250ms/s     0.78       +0.53
web-frontend           80ms/s    250ms/s     0.08       -0.17
payment-service        60ms/s    250ms/s     0.06       -0.19
redis-cache            50ms/s    150ms/s     0.05       -0.10
log-collector          30ms/s    100ms/s     0.03       -0.07

Only spark-executor has positive excess → sole candidate

Temporal correlation check (across 12 reports in 2m window):
  spark-executor intensity per interval:  [0.6, 0.65, 0.7, 0.75, 0.78, 0.8, 0.82, 0.78, 0.75, 0.8, 0.78, 0.77]
  max victim degradation per interval:    [3x, 4x, 6x, 8x, 11x, 13x, 14x, 11x, 9x, 12x, 11x, 10x]
  Pearson correlation: 0.89 → strong

Confidence calculation:
  base = min(0.53/0.25, 14.0/2.0, 12/6) = min(2.12, 7.0, 2.0) = 2.0 → clamped to 1.0
  temporal boost: 0.89 > 0.7 → multiply by 1.2 → 1.2 → clamped to 1.0
  combined check: single offender with 0.53 excess out of 0.78 total = 68% > 60% → no cap
  final confidence: 0.92

Action gating:
  0.92 >= 0.7 (taint) → YES
  0.92 >= 0.8 (cordon) → YES
  0.92 >= 0.9 (evict) → YES → but severity is CRITICAL, not EMERGENCY → eviction not in action set
  → Action: Taint + Event
```

---

## 6. Stage 5: Controller → Kubernetes — Action Execution

When the controller decides to act, it makes Kubernetes API calls:

```
Taint:
  PATCH /api/v1/nodes/worker-03
  Add taint: sentinel.io/noisy-neighbor=true:NoSchedule
  → Prevents new pods from being scheduled on this node
  → Existing pods are NOT evicted (NoSchedule, not NoExecute)
  → API call: ~50-100ms

Cordon:
  PATCH /api/v1/nodes/worker-03
  Set spec.unschedulable = true
  → Stronger than taint — no new pods at all
  → API call: ~50-100ms

Evict:
  POST /api/v1/namespaces/<ns>/pods/<pod>/eviction
  → Kubernetes eviction API respects PDB
  → Pod's controller (Deployment/ReplicaSet) creates replacement on another node
  → API call: ~100-200ms

Annotate:
  PATCH /api/v1/nodes/worker-03
  Set annotations: sentinel.io/severity, sentinel.io/contention-type, etc.
  Set labels: sentinel.io/health=degraded
  → API call: ~50-100ms (batched with taint/cordon patch)

Event:
  POST /api/v1/namespaces/sentinel-system/events
  → Creates K8s event visible via kubectl describe node
  → API call: ~50ms

Status CR:
  PUT /apis/sentinel.io/v1alpha1/nodecontentionstatuses/worker-03/status
  → Updates the NodeContentionStatus resource
  → API call: ~50-100ms
```

All API calls use the controller's ServiceAccount with scoped RBAC. Calls are retried with exponential backoff on failure. If the API server is unreachable, actions are queued in memory and executed when connectivity returns.

---

## 7. Stage 6: Recovery Loop

A separate reconciler runs every `recoveryCheckInterval` (default: 1 minute):

```
For each node with active sentinel actions (taint, cordon):
  1. Check latest ContentiontionReports
     → Is the metric that triggered the action now below WARNING threshold?
  2. Check de-escalation counter
     → Need 3 consecutive healthy readings (deescalationReadings)
  3. If both conditions met:
     a. Remove taint: PATCH node, remove sentinel.io/noisy-neighbor taint
     b. Uncordon: PATCH node, set spec.unschedulable = false
     c. Clear annotations: remove sentinel.io/* annotations
     d. Update NodeContentionStatus: phase = Healthy
     e. Emit event: ContentionResolved
```

Recovery is intentionally slower than escalation. Escalation is immediate (one evaluation cycle). De-escalation requires 3 consecutive healthy readings (3 × 10s evaluation interval = 30 seconds minimum). This asymmetry prevents flapping.

---

## 8. Scale Analysis — Per Component

All numbers in this section assume steady-state load, not burst spikes. During burst scenarios (sudden pod churn, correlated contention across many nodes, controller failover), overhead temporarily increases — see Section 10.4 for stress test methodology that covers those cases.

### 8.1 eBPF Programs (in-kernel)

The eBPF programs scale with **event rate**, not cluster size.

| Event | Typical rate | Handler cost | CPU overhead per core |
|-------|-------------|-------------|----------------------|
| sched_switch | 10K-50K/s/core | 200ns | 0.2% - 1.0% |
| block_rq_insert + complete | 1K-10K/s total | 150-200ns | negligible |
| tcp_retransmit_skb | 0-100/s total | 150ns | negligible |
| kfree_skb | 0-1K/s total | 100ns | negligible |

**sched_switch is the dominant cost.** On a busy 32-core node with 50K switches/s/core:

```
50,000 events/s × 200ns = 10ms/s per core = 1% of one core
Across 32 cores: 1% × 32 cores / 32 cores = 1% of total node CPU
```

On a 96-core high-performance node: ~0.33% of total CPU.

**Memory for BPF maps:**

```
Per-CPU hash map memory = max_entries × value_size × num_CPUs

sched_monitor:
  runq_latency_map: 4096 entries × 224 bytes × 32 CPUs = 28MB
  wakeup_ts_map: 65536 entries × 8 bytes = 512KB (not per-CPU)

blkio_monitor:
  blkio_latency_map: 4096 entries × 240 bytes × 32 CPUs = 30MB
  inflight_rq: 65536 entries × 24 bytes = 1.5MB

net_monitor:
  net_stats_map: 4096 entries × 48 bytes × 32 CPUs = 6MB

Total BPF map memory (32-core node, 4096 max_entries): ~67MB
Total BPF map memory (96-core node, 4096 max_entries): ~195MB
```

This is kernel memory, not charged to the agent pod's cgroup.

### 8.2 Go Agent (userspace, per-node)

| Operation | Cost | Frequency | CPU impact |
|-----------|------|-----------|------------|
| Map read + merge (all observers) | 10-15ms | every 5s | 0.2-0.3% of one core |
| Cgroup resolution (cache hit) | <1μs per lookup | every 5s per pod | negligible |
| Cgroup resolution (cache miss / CRI call) | 5-10ms per call | on pod create only | negligible |
| Histogram percentile computation | ~10μs per pod | every 5s | negligible |
| Baseline EMA update | ~1μs per pod per metric | every 5s | negligible |
| Ring buffer consumer | 0 when idle, ~1μs per event | event-driven | negligible |
| gRPC send | ~1ms per report | every 10s | negligible |
| Prometheus scrape response | 5-10ms | every 15-30s | negligible |

**Total agent CPU usage:** ~0.3% of one core. On a 32-core node: 0.01% of total.

**Agent memory:**

| Component | Size |
|-----------|------|
| Go runtime | 15MB |
| Cgroup resolver cache (250 pods) | 1MB |
| Baseline tracker (250 pods × 4 metrics × 64 bytes) | 64KB |
| gRPC buffers | 2MB |
| Ring buffer reader | 256KB |
| Prometheus metric state | 5MB |
| **Total** | **~24MB** |

### 8.3 Controller (cluster-wide)

| Operation | Cost per node | At 100 nodes | At 1000 nodes |
|-----------|--------------|-------------|---------------|
| gRPC receive + store | ~0.1ms | 10ms/s | 100ms/s |
| Evaluation cycle (all nodes) | 2-5ms per node | 200-500ms per 10s cycle | 2-5s per 10s cycle |
| API calls (on action) | 100-200ms per call | rare (few actions/hour) | rare |
| NodeContentionStatus updates | 50ms per node per cycle | 5s/cycle | 50s/cycle ← problem |

**Key insight:** At 1000 nodes, updating NodeContentionStatus CRs every 10 seconds becomes the bottleneck — 1000 × 50ms = 50 seconds, but we only have 10 seconds per cycle. Solutions:
- Only update CRs when state changes (not every cycle) — reduces to ~10-50 updates/cycle
- Batch updates using server-side apply
- Shard controller

**Controller memory:**

| Component | At 100 nodes | At 1000 nodes |
|-----------|-------------|---------------|
| Sliding window buffer | 2.4MB | 24MB |
| Per-node severity state | 1MB | 10MB |
| Per-pod baselines (from reports) | 5MB | 50MB |
| gRPC connection state | 2MB | 20MB |
| Policy cache | <1MB | <1MB |
| **Total** | **~11MB** | **~105MB** |

At 1000 nodes, the controller needs ~105MB. The 256MB limit leaves comfortable headroom.

---

## 9. Scale Tiers — What Works, What Breaks, What to Adapt

### Tier 1: Small (1-100 nodes, 30-250 pods/node)

**Everything works with defaults.** Single controller replica. No tuning needed.

| Metric | Value |
|--------|-------|
| Agent CPU per node | 0.01% of total |
| Agent memory per node | ~24MB |
| Controller CPU | < 5% of one core |
| Controller memory | ~11MB |
| gRPC traffic | 20KB/s |
| End-to-end detection latency | 15-20s |
| BPF map kernel memory per node | 67MB (32-core) |

### Tier 2: Medium (100-500 nodes, 30-250 pods/node)

**Works with defaults.** Single controller is fine. Monitor `sentinel_evaluation_latency_seconds` to watch for early pressure signs.

| Metric | Value |
|--------|-------|
| Controller CPU | < 25% of one core |
| Controller memory | ~55MB |
| gRPC traffic | 100KB/s |
| Evaluation cycle time | ~1-2.5s of 10s budget |

### Tier 3: Large (500-2000 nodes, 30-250 pods/node)

**Controller needs sharding.** 2-3 replicas with consistent hash partitioning.

| Metric | Value (unsharded) | Value (3 shards) |
|--------|------------------|------------------|
| Controller CPU per replica | 50-100% of one core | 17-33% of one core |
| Controller memory per replica | 105-210MB | 35-70MB |
| Evaluation cycle time | 5-10s (tight) | 1.7-3.3s (comfortable) |
| gRPC connections per replica | 500-2000 | 167-667 |

**Required changes:**
- Enable controller sharding: `--shard-count=3`
- Each replica takes nodes where `hash(node_name) % 3 == replica_id`
- Leader election is per-shard (each replica is leader of its own partition)
- NodeContentionStatus updates only on state change (not every cycle)

### Tier 4: Very Large (2000+ nodes, 30-250 pods/node)

**Label-based partitioning.** Split by zone, node pool, or hardware class.

```yaml
# Controller instance 1: zone-a
--partition-label=topology.kubernetes.io/zone
--partition-value=us-east-1a

# Controller instance 2: zone-b
--partition-label=topology.kubernetes.io/zone
--partition-value=us-east-1b
```

Each controller only watches nodes matching its partition. Agents connect to the controller responsible for their node's zone.

### Tier 5: Extreme Pod Density (any node count, 1000+ pods/node)

**Agent-side adaptations required.** This is the 10K pods/node scenario discussed earlier.

| Problem | Solution | Trade-off |
|---------|----------|-----------|
| BPF map max_entries too small (4096) | Increase to 16384 | 4× kernel memory per map |
| Per-CPU map memory explodes (~1GB+ per map on 96-core) | Switch to non-per-CPU maps for less critical observers (net, mm) | Adds lock contention to those BPF programs |
| Cgroup resolver churn (hundreds of creates/deletes per second) | Batch CRI API calls, increase rescan interval, add LRU eviction | Slightly delayed resolution for new pods |
| ContentionReport too large if including all pods | Already handled: top-K filtering (top 50 pods per report) | Lose visibility into well-behaved pods |
| Map read takes too long (400ms at 10K entries) | Increase agent poll interval to 10-15s, or read maps incrementally | Slightly higher detection latency |

**When per-CPU maps hit memory limits:**

```
Per-CPU hash map at 16K entries on 96-core node:
  16,384 × 224 bytes × 96 CPUs = 335MB per map
  4 observer maps × 335MB = 1.3GB total kernel memory

Alternative: namespace-level aggregation
  ~200 namespaces instead of 10K cgroups
  200 × 224 bytes × 96 CPUs = 4MB per map
  4 maps × 4MB = 16MB total kernel memory
  
  Trade-off: attribution is per-namespace, not per-pod
  "batch-jobs namespace is the noisy neighbor" instead of "pod-xyz is the noisy neighbor"
```

---

## 10. Benchmark Methodology

### 10.1 Overhead Benchmark

**Goal:** Validate that agent overhead stays within budget at each scale tier.

**Setup:**
- 3 identical nodes (same hardware, same OS, same kernel)
- Node A: baseline (no sentinel)
- Node B: sentinel agent running with all observers
- Node C: sentinel agent + synthetic contention (stress-ng)
- Workload: standardized (nginx serving traffic + postgres under load + batch job)
- Duration: 1 hour per test

**Metrics to capture:**

| Metric | Source | Acceptable delta |
|--------|--------|-----------------|
| Application p99 latency | nginx/postgres metrics | < 1% increase |
| Total node CPU utilization | node_exporter | < 1% increase |
| Agent RSS memory | cgroup metrics | < 50MB |
| BPF program execution time | bpf_prog_info (kernel) | < 200ns/invocation avg |
| BPF map kernel memory | /proc/meminfo or bpftool | < 200MB (32-core) |
| Map read latency | sentinel agent histogram | < 50ms per cycle |

**Procedure:**
```
1. Deploy identical workload on all 3 nodes
2. Wait 10 minutes for warm-up
3. Record baseline metrics from Node A for 1 hour
4. Record metrics from Node B for 1 hour (agent running, no contention)
5. Compare: B metrics should be within tolerance of A metrics
6. Deploy stress-ng on Node C: 
   stress-ng --cpu 4 --io 8 --vm 2 --vm-bytes 1G --timeout 1h
7. Record metrics from Node C for 1 hour
8. Verify: agent correctly detected contention, overhead still within budget
```

### 10.2 Detection Accuracy Benchmark

**Goal:** Measure true positive rate, false positive rate, and detection latency.

**Setup:**
- 5-node cluster
- Synthetic contention injected with known parameters (ground truth)

**Test matrix:**

| Test | Injection | Expected result |
|------|-----------|----------------|
| CPU single offender | stress-ng --cpu 8 on one pod | Offender correctly identified, confidence > 0.8 |
| IO single offender | stress-ng --io 16 on one pod | Offender correctly identified, confidence > 0.8 |
| CPU multi-offender | stress-ng --cpu 4 on two pods | Both identified, ranked by intensity |
| Diffuse contention | stress-ng --cpu 2 on five pods | Confidence capped at 0.5, alert only |
| No contention | Normal workload | No alerts, severity=Healthy |
| Short burst | stress-ng --timeout 10s | Filtered by burstFilterWindow, no action |
| System process contention | stress-ng run outside cgroup | Labeled "unknown", not attributed |

**Metrics:**

```
True Positive Rate  = correctly identified offender / total actual offenders
False Positive Rate = incorrectly blamed pods / total non-offender pods
Detection Latency   = time from contention start to first WARNING event
Attribution Latency = time from contention start to offender identification
Confidence Accuracy = correlation between confidence score and actual ground truth
```

### 10.3 Scale Benchmark

**Goal:** Find the breaking point for each component and validate the tier analysis.

**Setup:**
- Use kwok (Kubernetes WithOut Kubelet) to simulate large clusters
- Real agents on real nodes for BPF benchmarks (kwok doesn't run BPF)
- Synthetic ContentionReports fed to controller via gRPC for controller benchmarks

**Agent scale test:**

```
Variables: pods per node (50, 100, 250, 500, 1000, 2000, 5000)
On a real 32-core node:
  1. Create N pods (sleep containers)
  2. Run stress-ng on one pod
  3. Measure:
     - Map read time per cycle
     - Cgroup resolution time
     - Agent CPU and memory
     - Report size
  4. Record the pod count where map read time exceeds 500ms per cycle
     → This is the agent's practical scaling limit
```

**Controller scale test:**

```
Variables: node count (100, 500, 1000, 2000, 5000)
  1. Deploy controller (single replica)
  2. Launch N synthetic agents (Go programs sending fake ContentionReports)
  3. Measure:
     - Evaluation cycle duration
     - gRPC receive throughput
     - Controller CPU and memory
     - API call latency (mocked K8s API server)
  4. Record the node count where evaluation cycle exceeds 8s (80% of 10s budget)
     → This is the single-controller scaling limit
  5. Repeat with 2, 3, 5 controller replicas (sharded)
     → Validate linear scaling
```

**End-to-end scale test:**

```
On a 10-node real cluster:
  1. Deploy sentinel (agents + controller)
  2. Inject contention on 3 nodes simultaneously
  3. Measure:
     - Detection latency per node
     - Attribution accuracy per node
     - Cluster safety limits respected (no more than 10% nodes tainted)
  4. Kill controller pod, verify recovery:
     - How long until new leader takes over?
     - Do existing taints survive?
     - How long until evaluation resumes?
```

### 10.4 Stress Test — Worst Case Scenarios

| Scenario | What it tests | How to inject |
|----------|--------------|---------------|
| All nodes contended simultaneously | Cluster safety limits | stress-ng on every node |
| Agent OOMKilled under load | Agent recovery and data gap handling | Set agent memory limit to 32MB and create 500 pods |
| Controller leader failover | Recovery time and state reconstruction | kubectl delete pod controller-leader |
| gRPC disconnection during action | Action completion and retry logic | Network policy blocking agent→controller |
| Rapid pod churn (100 creates/deletes per second) | Cgroup resolver stability | Script that creates and deletes pods in a loop |
| BPF map full | Agent degradation behavior | Set max_entries=100 with 200 pods |

---

## Summary — Key Numbers to Remember

| Metric | Value |
|--------|-------|
| **End-to-end detection latency** | 15-20 seconds |
| **Agent CPU overhead** | < 1% of total node CPU |
| **Agent memory** | ~24MB userspace + ~67MB kernel (32-core) |
| **Controller capacity (single)** | ~500-1000 nodes |
| **Controller capacity (3 shards)** | ~1500-3000 nodes |
| **Max pods/node (default config)** | ~500 (comfortable), ~1000 (with tuning) |
| **gRPC report size** | ~2-5KB per node per 10s |
| **BPF handler execution time** | 100-200ns per event |
| **Evaluation time per node** | 2-5ms |
| **Recovery time after controller crash** | ~2 minutes (evaluation window refill) |
| **Maximum concurrent tainted nodes** | 10% of cluster (configurable) |

---

*End of Document*
