# node-sentinel — Design Document

## eBPF-Powered Noisy Neighbor Detection Operator for Kubernetes

**Status:** Draft v0.3.1
**Author:** Brajesh Pant
**Date:** April 2026

---

## Executive Summary

Kubernetes clusters share node-level resources (CPU run queues, disk I/O queues, NIC queues) across pods, but the scheduler has no visibility into contention at these boundaries. When one pod saturates a shared resource, co-located pods degrade silently — detected only when SLOs break. **node-sentinel** uses eBPF to observe contention signals inside the kernel, attributes degradation to specific pods, and automatically remediates via taints, cordons, or evictions — all governed by operator-defined CRD policies with confidence-gated safety guardrails. It also feeds contention data back to the scheduler to prevent recurrence.

Unlike traditional monitoring tools, node-sentinel identifies **multiple contributing offenders** and assigns **confidence scores** before taking any action — the higher the risk of the action (eviction > cordon > taint), the higher the confidence required.

```
[ eBPF (kernel) ] ──► [ Agent (node) ] ──► [ Controller (cluster) ] ──► [ Action (taint/cordon/evict) ]
```

---

## Quick Navigation

| If you want to understand... | Go to |
|------------------------------|-------|
| The problem we're solving | Section 1 |
| System architecture | Section 6 |
| How decisions are made | Section 6.6 — Decision Engine |
| How offenders are identified | Section 7.5 — Attribution Algorithm |
| How eviction is kept safe | Section 7.6 — Eviction Safety Model |
| What happens when sentinel is wrong | Section 7.6 + Section 12.4 — Misclassification Handling |
| Sample policy configs | Section 20 |
| How to debug issues | Section 21 |

---

## Terminology

| Term | Definition |
|------|-----------|
| **Contention** | A condition where multiple pods compete for a shared node resource (CPU scheduler, I/O queue, NIC queue), causing performance degradation beyond what cgroup limits control. Distinct from *usage* — a pod can be within its CPU limit but still suffer contention from a neighbor monopolizing the run queue. |
| **Offender** | A pod whose disproportionate resource consumption correlates with degradation of other pods. Identified probabilistically via the attribution algorithm. A node may have multiple offenders. |
| **Victim** | A pod experiencing measurable performance degradation (latency increase, throughput decrease) correlated with an offender's resource intensity. |
| **Confidence** | A score (0.0–1.0) representing how certain the attribution algorithm is that a specific pod is the offender. Gates which remediation actions are permitted — higher-risk actions (eviction) require higher confidence. |
| **Observer** | An eBPF program that hooks into a kernel subsystem to collect contention metrics for a specific resource type (CPU scheduling, block I/O, network, memory, NUMA). |
| **Contention Report** | A periodic summary (default: every 10s) sent from the agent to the controller, containing per-pod contention metrics, anomaly flags, and offender candidates for that node. |

---

## Maturity Tags

Each section is tagged with a maturity level to indicate its readiness and build priority:

| Tag | Meaning |
|-----|---------|
| `[Core]` | Essential to the system. Must be correct and complete before any deployment. Built in Phases 1–3. |
| `[Extended]` | Designed and validated but ships later. Enhances the system for advanced use cases. Built in Phase 4. |
| `[Experimental]` | Designed but may change significantly based on real-world data. Built in Phase 5 or deferred. |

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [System Boundaries](#2-system-boundaries)
3. [Why eBPF (and Why Not Existing Tools)](#3-why-ebpf-and-why-not-existing-tools)
4. [Goals and Non-Goals](#4-goals-and-non-goals)
5. [Design Trade-offs](#5-design-trade-offs)
6. [High-Level Design (HLD)](#6-high-level-design-hld)
   - 6.1 System Overview
   - 6.2 Component Architecture
   - 6.3 Operator Experience
   - 6.4 Data Flow
   - 6.5 CRD Design
   - 6.6 Decision Engine
   - 6.7 Scheduler Feedback Loop `[Extended]`
   - 6.8 Deployment Topology
7. [Low-Level Design (LLD)](#7-low-level-design-lld)
   - 7.1 eBPF Programs
   - 7.2 Go Agent (node-sentinel-agent)
   - 7.3 Controller (node-sentinel-controller)
   - 7.4 Cgroup-to-Pod Resolution
   - 7.5 Anomaly Detection and Attribution Algorithm
   - 7.6 Eviction Safety Model
   - 7.7 Taint/Cordon Lifecycle
   - 7.8 eBPF Map Design
   - 7.9 Ring Buffer Protocol
   - 7.10 NUMA-Aware Contention Detection `[Extended]`
   - 7.11 Agent CLI — sentinelctl
8. [Controller State Model](#8-controller-state-model)
9. [Data Freshness and Timing Guarantees](#9-data-freshness-and-timing-guarantees)
10. [Scaling](#10-scaling)
11. [Kernel Compatibility](#11-kernel-compatibility)
12. [Failure Modes and Recovery](#12-failure-modes-and-recovery)
13. [Security Model](#13-security-model)
14. [Observability](#14-observability)
15. [Extensibility — Observer Plugin Model](#15-extensibility--observer-plugin-model) `[Experimental]`
16. [Performance Overhead Budget](#16-performance-overhead-budget)
17. [Real-World Scenarios](#17-real-world-scenarios)
18. [Known Limitations](#18-known-limitations)
19. [Versioning and Compatibility](#19-versioning-and-compatibility)
20. [Sample Configurations](#20-sample-configurations)
21. [Debugging and Troubleshooting Workflows](#21-debugging-and-troubleshooting-workflows)
22. [Benchmarking and Validation Plan](#22-benchmarking-and-validation-plan)
23. [Implementation Phases](#23-implementation-phases)

---

## 1. Problem Statement

In multi-tenant Kubernetes clusters, pods share underlying node resources — CPU, memory bandwidth, disk I/O, and network. Kubernetes resource limits (cgroups v2) provide basic isolation, but they don't prevent contention at shared resources that the scheduler doesn't model: memory bus bandwidth, disk I/O queue depth, CPU cache pressure, and NIC queue saturation.

A "noisy neighbor" is a pod that disproportionately consumes a shared resource, degrading the performance of co-located pods. This manifests as:

- **CPU scheduling latency**: Pod A is CPU-bound and causes Pod B's threads to wait longer in the run queue, even though Pod B hasn't hit its CPU limit.
- **Block I/O starvation**: Pod A doing sequential bulk writes saturates the disk controller, causing Pod B's fsync operations to spike from 1ms to 200ms.
- **Network queue contention**: Pod A generates high packet rates, causing increased tail latency and TCP retransmissions for Pod B.
- **Memory bandwidth / cache thrashing**: Pod A's memory access patterns evict Pod B's hot cache lines (harder to observe, but real on bare-metal).

Today, these problems are detected reactively — someone notices degraded application latency, SSHes into the node, and manually investigates. By that time, SLOs are already broken. **Engineers know a pod is slow — but not which neighbor caused it.** As multi-tenant clusters grow denser — more pods per node, more teams sharing infrastructure, more latency-sensitive workloads — contention becomes more frequent and harder to debug manually.

**node-sentinel** automates this entire loop: detect contention at the kernel level using eBPF, attribute it to specific pods, evaluate it against operator-defined policies, and take corrective action (taint, cordon, evict, or alert). It also feeds contention signals back to the scheduler to prevent recurrence.

---

## 2. System Boundaries

node-sentinel operates at the **kernel-infrastructure layer**. This section defines exactly what it sees and what it cannot see.

**What the system detects (kernel-level contention):**
- CPU scheduling latency caused by run queue saturation
- Block I/O latency caused by device queue contention
- Network degradation caused by NIC queue saturation, TCP retransmissions, packet drops
- Memory pressure caused by direct reclaim storms (with lower attribution precision)
- NUMA-localized contention on multi-socket hardware

**What the system does NOT detect (out of scope):**
- **Application-level contention**: Two pods competing for the same external database, Redis key conflicts, shared file locks, distributed lock contention. These don't manifest as kernel contention signals.
- **Network contention beyond the node**: Upstream switch saturation, cross-node bandwidth limits, load balancer bottlenecks. node-sentinel sees the local NIC queue, not the network fabric.
- **Storage backend contention**: If a remote NFS/Ceph/EBS volume is slow, the kernel sees I/O latency but the cause is external. Attribution may incorrectly flag a local pod. The operator should exclude remote-storage-backed pods from offender detection in such environments.
- **CPU cache and memory bus contention**: LLC (Last-Level Cache) eviction between pods is real on bare-metal but has no kernel tracepoint. We detect the downstream effect (higher memory access latency → higher run queue time) but cannot attribute it to cache specifically.
- **GPU, FPGA, or accelerator contention**: No eBPF hooks exist for vendor-specific accelerator drivers. The plugin model (Section 15) allows adding custom observers if vendor tracepoints become available.

**Where attribution becomes unreliable:**
- When multiple pods spike simultaneously and their resource profiles are similar (diffuse contention)
- When contention is caused by system-level processes (kernel threads, systemd services) rather than pods
- When memory pressure triggers global reclaim (all cgroups affected, hard to isolate cause)

In these cases, the system caps confidence and falls back to alerting instead of automated action. See Section 7.5 for the full attribution algorithm and its limitations.

---

## 3. Why eBPF (and Why Not Existing Tools)

### Why not cAdvisor / metrics-server / Prometheus node-exporter?

These tools expose **cgroup-level resource accounting**: CPU usage, memory RSS, network bytes. They tell you *how much* a pod consumed. They do **not** tell you:

- How long Pod B's threads waited in the CPU run queue because Pod A was hogging cores (scheduling latency)
- How much Pod B's fsync latency increased because Pod A saturated the disk controller (I/O queue contention)
- Whether TCP retransmits for Pod B spiked because Pod A flooded the NIC queue

These are **contention signals**, not usage signals. Contention happens at kernel subsystem boundaries — the scheduler run queue, the block I/O request queue, the network device queue. The only way to observe them is inside the kernel.

### Why not Falco / Tetragon?

Falco and Tetragon use eBPF for **security runtime detection** — unexpected syscalls, file access, process execution. They're designed to answer "is something malicious happening?" not "is something causing performance degradation to its neighbors?" Their event models, rule engines, and action systems are optimized for security policy enforcement, not resource contention analysis. Building noisy neighbor detection on top of them would mean fighting their abstractions.

### Why not Cilium / Hubble?

Cilium provides excellent **network-level** observability. But noisy neighbor problems span CPU, disk, memory, and network. Cilium can tell you about network contention, but not that a pod's CPU-intensive workload is starving its neighbors of scheduler time, or that a pod's write-heavy workload is saturating the disk. node-sentinel complements Cilium — it covers the non-network contention dimensions.

### Why eBPF specifically?

eBPF gives us programmable, safe, low-overhead hooks into kernel subsystems with zero application changes:

- Attach to scheduler tracepoints to measure run queue latency per cgroup
- Attach to block layer tracepoints to measure I/O latency per cgroup
- Attach to TCP/IP kprobes to measure retransmit rates per cgroup
- All with kernel-verified safety (cannot crash the kernel) and negligible overhead (in-kernel aggregation avoids copying raw events to userspace)

No other technology provides this combination of depth, safety, and performance.

**node-sentinel is not a monitoring tool — it is a control system.** Monitoring tools tell you what happened. node-sentinel detects, attributes, decides, and acts — closing the loop from kernel signal to cluster remediation without human intervention.

---

## 4. Goals and Non-Goals

### Goals

- Detect noisy neighbor conditions using kernel-level signals (not just cgroup metrics)
- Attribute resource contention to specific pods, handling single and multi-offender scenarios
- Support operator-defined policies via CRDs (thresholds, actions, cooldowns)
- Automatically remediate by tainting/cordoning nodes or evicting offending pods with safety guardrails
- Feed contention signals back to the scheduler via node annotations and topology hints
- Work on both on-prem bare-metal and cloud-managed Kubernetes (kernel >= 5.10)
- Support NUMA-aware contention detection on multi-socket bare-metal nodes
- Provide Prometheus metrics, Kubernetes events, and a node-local CLI for observability
- Maintain minimal overhead: agent < 1% CPU and < 50MB RSS under normal conditions (see Section 16 for justification)
- Support an extensible observer model for adding custom contention signals

### Non-Goals

- Replacing the Kubernetes scheduler (we inform and influence, not schedule)
- Providing a full network observability solution (Cilium/Hubble handles that)
- Supporting kernels < 5.10 or cgroups v1 (design assumes cgroups v2)
- Real-time packet inspection or L7 protocol analysis
- Windows node support
- Replacing Falco/Tetragon for security runtime detection

---

## 5. Design Trade-offs

These are deliberate engineering choices with understood consequences:

**Accuracy vs. Overhead** — We chose in-kernel histogram aggregation (log2 buckets) over raw event streaming. This means we lose per-event granularity but reduce userspace data transfer by ~1000x. For 5-10 second evaluation windows, histogram-derived percentiles are sufficiently accurate.

**Attribution Precision vs. Safety** — Attribution is probabilistic, not deterministic (see Section 7.5). When confidence is low, the system alerts but does not act. False positives (blaming the wrong pod) are treated as more dangerous than false negatives (missing a noisy neighbor). This means some contention events go unattributed — that's by design.

**Real-time vs. Batch** — Contention reports are aggregated over 5-second intervals rather than streamed per-event. This reduces gRPC traffic and controller load, at the cost of 5-second detection latency. For a system that applies taints (not kills processes), this is acceptable.

**Single Binary vs. Kernel Module Dependency** — We use CO-RE (Compile Once, Run Everywhere) to ship a single agent binary with embedded BPF bytecode. This avoids the operational pain of matching kernel headers at deployment time, at the cost of requiring kernels with BTF support (5.5+, universally available on 5.10+ LTS).

**Conservative Eviction vs. Aggressive Remediation** — The eviction path has multiple guardrails (workload type checks, PDB respect, allow/deny lists). This means some noisy neighbors survive longer than they should. The alternative — aggressive eviction — risks taking out stateful workloads or critical services, which is strictly worse.

**Per-Node Agent vs. Centralized eBPF Management** — Each node runs its own agent that loads and manages its own eBPF programs. This is operationally simpler than a centralized BPF management plane, but means each node independently consumes resources for BPF program execution. At typical pod densities (30-250 pods/node), the per-node overhead is negligible.

---

## 6. High-Level Design (HLD)

> This document describes the complete system design. Core implementation is limited to `[Core]` sections in Phases 1–3. Sections tagged `[Extended]` and `[Experimental]` are fully designed but ship in later phases.

### 6.1 System Overview `[Core]`

node-sentinel is a three-component system: per-node agents, a cluster controller, and a scheduler integration layer. Operators can inspect live contention using `sentinelctl top`, which shows offender and victim pods in real time on any node.

```
┌──────────────────────────────────────────────────────────────────┐
│                       Kubernetes Cluster                          │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │              node-sentinel-controller                     │    │
│  │              (Deployment, 1-2 replicas)                   │    │
│  │                                                           │    │
│  │  ┌──────────┐ ┌──────────┐ ┌───────────┐ ┌───────────┐  │    │
│  │  │ CRD      │ │ Decision │ │ Action    │ │ Scheduler │  │    │
│  │  │ Watcher  │ │ Engine   │ │ Executor  │ │ Annotator │  │    │
│  │  └──────────┘ └──────────┘ └───────────┘ └───────────┘  │    │
│  │                      ▲                                    │    │
│  └──────────────────────┼────────────────────────────────────┘    │
│                         │ gRPC (Contention Reports)                │
│         ┌───────────────┼───────────────┐                         │
│         │               │               │                         │
│  ┌──────┴──────┐ ┌──────┴──────┐ ┌──────┴──────┐                │
│  │   Agent     │ │   Agent     │ │   Agent     │                │
│  │  (Node A)   │ │  (Node B)   │ │  (Node C)   │                │
│  │             │ │             │ │             │                │
│  │  eBPF progs │ │  eBPF progs │ │  eBPF progs │                │
│  │  ┌────────┐ │ │  ┌────────┐ │ │  ┌────────┐ │                │
│  │  │sched   │ │ │  │sched   │ │ │  │sched   │ │                │
│  │  │blkio   │ │ │  │blkio   │ │ │  │blkio   │ │                │
│  │  │net     │ │ │  │net     │ │ │  │net     │ │                │
│  │  │mm      │ │ │  │mm      │ │ │  │mm      │ │                │
│  │  └────────┘ │ │  └────────┘ │ │  └────────┘ │                │
│  │             │ │             │ │             │                │
│  │ sentinelctl │ │ sentinelctl │ │ sentinelctl │                │
│  │  DaemonSet  │ │  DaemonSet  │ │  DaemonSet  │                │
│  └─────────────┘ └─────────────┘ └─────────────┘                │
│                                                                   │
│  ┌──────────────────────────────────────────────────────────┐    │
│  │  sentinel-scheduler-plugin (optional)                     │    │
│  │  OR: annotation-based integration with default scheduler  │    │
│  └──────────────────────────────────────────────────────────┘    │
│                                                                   │
└──────────────────────────────────────────────────────────────────┘
```

### 6.2 Component Architecture `[Core]`

#### Component 1: node-sentinel-agent (DaemonSet)

Runs on every node. Responsible for:
- Loading and managing eBPF programs attached to kernel tracepoints/kprobes
- Reading eBPF maps and ring buffers at configurable intervals
- Aggregating raw kernel events into per-pod contention metrics
- Resolving cgroup IDs to pod identities
- NUMA topology discovery and per-NUMA-node metric aggregation `[Extended]`
- Reporting contention summaries to the controller via gRPC stream
- Exposing per-node Prometheus metrics
- Serving local gRPC API for `sentinelctl` CLI

The agent is **self-contained** — it can operate in standalone mode (metrics + events only, no automated actions) even if the controller is unreachable.

#### Component 2: node-sentinel-controller (Deployment)

Cluster-level singleton (with leader election). Responsible for:
- Watching `NodeHealthPolicy` CRDs for operator-defined thresholds and actions
- Receiving contention reports from all agents
- Evaluating reports against policies using sliding window analysis
- Multi-offender attribution and confidence scoring
- Executing remediation actions (taint, cordon, evict) with safety guardrails
- Managing action lifecycle (cooldown, auto-recovery, escalation)
- Annotating nodes with contention type hints for scheduler integration `[Extended]`
- Emitting cluster-level Prometheus metrics and Kubernetes events

#### Component 3: Scheduler Integration Layer `[Extended]`

Feeds contention data back into scheduling decisions to prevent recurrence:
- **Annotation-based (default):** Controller annotates nodes with contention type labels. Operators configure pod anti-affinity or topology spread constraints to avoid hot nodes.
- **Scheduler plugin (advanced):** A custom scheduler plugin reads node contention scores and deprioritizes nodes with active contention during the Score phase.

### 6.3 Operator Experience `[Core]`

Operators interact with node-sentinel through four interfaces:

**1. CRD-driven policy:** Define `NodeHealthPolicy` resources to set thresholds, actions, and safety guardrails. Policies support three modes — `observe` (metrics only), `alert` (metrics + events), and `enforce` (full automated remediation) — enabling safe incremental rollout.

**2. sentinelctl CLI:** SSH into any node's agent pod and run `sentinelctl top` for a live, htop-style view of per-pod contention. Useful for incident investigation and validating that the system sees what you expect.

**3. Kubernetes events and annotations:** The controller emits structured K8s events for every detection, action, denial, and recovery. Node annotations provide at-a-glance status (`sentinel.io/severity`, `sentinel.io/contention-type`, `sentinel.io/offenders`).

**4. Prometheus metrics + dashboards:** Both agent and controller expose detailed Prometheus metrics. A reference Grafana dashboard ships with the Helm chart, showing per-node contention heatmaps, attribution confidence trends, and action history.

### 6.4 Data Flow `[Core]`

```
  Kernel Event (e.g., sched_switch fires)
       │
       ▼
  eBPF Program (in-kernel)
  ├── computes delta (e.g., run queue latency)
  ├── attributes to cgroup_id
  ├── optionally attributes to NUMA node (via cpu_id → NUMA mapping)
  ├── updates per-CPU hash map: key=(cgroup_id, metric_type), value=histogram
  └── if threshold breached → pushes event to ring buffer
       │
       ▼
  Go Agent (userspace)
  ├── periodic map reader (every 5s default)
  │   ├── reads and merges per-CPU map data
  │   ├── resolves cgroup_id → (namespace, pod, container)
  │   └── aggregates and analyzes metrics per pod
  │
  ├── ring buffer consumer (event-driven)
  │   └── processes threshold-breach events pushed from kernel
  │
  ├── anomaly detector
  │   ├── compares current metrics against rolling baseline
  │   ├── filters transient noise and flags sustained anomalies
  │   └── builds ContentionReport proto
  │
  ├── sentinelctl gRPC server (local, unix socket)
  │   └── serves live contention data for on-node debugging
  │
  └── gRPC stream → Controller
       │
       ▼
  Controller
  ├── receives ContentionReport per node
  ├── evaluates against NodeHealthPolicy
  │   ├── check: is any metric above warning threshold?
  │   ├── check: has it persisted beyond evaluationWindow?
  │   ├── check: is cooldown active from previous action?
  │   └── check: which action tier applies? (warning → critical → emergency)
  │
  ├── attribution engine
  │   ├── identifies top-N offenders (not just single pod)
  │   ├── computes confidence score per offender
  │   └── confidence < threshold → alert only, no action
  │
  ├── safety checks (for eviction)
  │   ├── is offender a StatefulSet / DaemonSet?
  │   ├── is offender in the eviction deny list?
  │   ├── would eviction violate PDB?
  │   └── fallback: cordon-only if eviction is unsafe
  │
  ├── executes action
  │   ├── warning: emit K8s Event + metric
  │   ├── critical: taint node with NoSchedule
  │   ├── emergency: cordon node + evict offender (if safe)
  │   └── all actions: annotate node with sentinel metadata
  │
  ├── scheduler feedback
  │   ├── annotates node: sentinel.io/contention-type, sentinel.io/contention-score
  │   └── updates node labels for topology-aware scheduling
  │
  └── recovery loop
      ├── periodically re-evaluates tainted/cordoned nodes
      ├── if contention subsided → remove taint / uncordon / clear annotations
      └── emits recovery event
```

### 6.5 CRD Design `[Core]`

#### NodeHealthPolicy (cluster-scoped)

Defines what to observe, thresholds, and what actions to take.

```yaml
apiVersion: sentinel.io/v1alpha1
kind: NodeHealthPolicy
metadata:
  name: default
spec:
  # Which nodes this policy applies to (label selector)
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""

  # Policy priority — higher number wins when multiple policies match a node
  # This resolves multi-policy conflicts deterministically
  priority: 100

  # Operating mode
  mode: enforce    # observe | alert | enforce
  # observe: collect metrics only, no events or actions
  # alert: emit events and metrics, no taints/cordons/evictions
  # enforce: full automated remediation

  # Observation configuration
  observers:
    cpu:
      enabled: true
      runQueueLatency:
        enable: true
      schedDelay:
        enable: true
    blockIO:
      enabled: true
      ioLatency:
        enable: true
      ioThroughput:
        enable: true
    network:
      enabled: true
      tcpRetransmits:
        enable: true
      packetDrops:
        enable: true
    memory:
      enabled: false
      pageFaults:
        enable: false
    numa:
      enabled: false  # enable per-NUMA-node tracking
      crossNodeLatency:
        enable: false

  # Thresholds — what constitutes a noisy neighbor
  # Values represent the victim-side impact (not the offender's usage)
  thresholds:
    warning:
      cpuRunQueueLatencyP99: 20ms
      ioLatencyP99: 100ms
      tcpRetransmitRate: 3%
    critical:
      cpuRunQueueLatencyP99: 50ms
      ioLatencyP99: 300ms
      tcpRetransmitRate: 8%
    emergency:
      cpuRunQueueLatencyP99: 100ms
      ioLatencyP99: 500ms
      tcpRetransmitRate: 15%

  # Validation constraints (webhook-enforced)
  # warning < critical < emergency for each metric (reject otherwise)
  # All durations must be >= 10ms
  # All rates must be >= 1%

  # Anomaly detection tuning
  anomalyDetection:
    baselineWindow: 30m          # EMA window for establishing per-pod baseline
    deviationSigma: 3.0          # standard deviations above baseline to flag
    burstFilterWindow: 15s       # ignore contention shorter than this (noise)
    smoothingAlpha: 0.3          # EMA smoothing factor (0=no smoothing, 1=no memory)

  # Attribution configuration
  attribution:
    maxOffenders: 3              # track top N offenders, not just one
    confidenceThreshold: 0.7     # below this → alert only, never act
    # confidence tiers:
    # < 0.5: no report
    # 0.5 - 0.7: report as "low confidence", alert only
    # 0.7 - 0.9: report as "medium confidence", taint allowed
    # > 0.9: report as "high confidence", all actions allowed
    actionConfidenceOverrides:
      taint: 0.7
      cordon: 0.8
      evict: 0.9                 # eviction requires highest confidence

  # How long a threshold must be breached before action
  evaluation:
    window: 2m
    interval: 10s
    minSamples: 6

  # What to do at each severity level
  actions:
    warning:
      - type: Event
      - type: Metric
      - type: SchedulerAnnotation   # annotate node with contention hints
    critical:
      - type: Taint
        key: sentinel.io/noisy-neighbor
        value: "true"
        effect: NoSchedule
      - type: SchedulerAnnotation
      - type: Event
    emergency:
      - type: Cordon
      - type: Evict
        target: offender
        gracePeriod: 30s
      - type: SchedulerAnnotation
      - type: Event

  # Eviction safety guardrails
  evictionPolicy:
    # Workloads that can be evicted
    allow:
      ownerKinds:
        - Deployment
        - ReplicaSet
        - Job
      labelSelector:
        matchLabels:
          sentinel.io/evictable: "true"
    # Workloads that must NEVER be evicted (overrides allow)
    deny:
      ownerKinds:
        - StatefulSet
        - DaemonSet
      namespaces:
        - kube-system
        - sentinel-system
      labelSelector:
        matchLabels:
          sentinel.io/critical: "true"
    # Respect PodDisruptionBudgets
    respectPDB: true
    # If eviction is denied, fall back to this action
    fallbackAction: Cordon

  # Cooldown and recovery
  lifecycle:
    cooldownAfterAction: 5m
    recoveryCheckInterval: 1m
    autoRecover: true
    maxActionsPerHour: 5              # per-node cap
    # De-escalation requires N consecutive healthy readings
    deescalationReadings: 3

  # Cluster-wide blast radius limits
  clusterSafety:
    maxConcurrentTaintedNodes: 10%    # percentage of total worker nodes
    maxConcurrentCordonedNodes: 5%    # more restrictive for cordon
    maxEvictionsPerHour: 10           # cluster-wide eviction cap
    # If limit reached → new actions queued, not dropped
    # Existing actions remain — only new ones are deferred

  # Pods to exclude from being flagged as offenders
  exclusions:
    namespaces:
      - kube-system
      - sentinel-system
    labelSelector:
      matchLabels:
        sentinel.io/exempt: "true"

  # Scheduler feedback configuration
  schedulerFeedback:
    enabled: true
    # Annotation format on nodes:
    #   sentinel.io/contention-type: "cpu,io"
    #   sentinel.io/contention-score: "0.8"
    #   sentinel.io/contention-since: "2026-04-07T10:14:00Z"
    # Labels (for nodeAffinity / topology spread):
    #   sentinel.io/health: "degraded"  (or "healthy")
    clearOnRecovery: true
```

#### NodeContentionStatus (node-scoped, status subresource)

Controller-managed resource reflecting current contention state per node. Read-only for operators, written by the controller.

```yaml
apiVersion: sentinel.io/v1alpha1
kind: NodeContentionStatus
metadata:
  name: worker-node-03
spec:
  nodeRef: worker-node-03
status:
  phase: Critical        # Healthy | Warning | Critical | Emergency
  mode: enforce          # current policy mode
  lastEvaluated: "2026-04-07T10:15:30Z"
  activeActions:
    - type: Taint
      appliedAt: "2026-04-07T10:14:00Z"
      reason: "Sustained CPU run queue latency p99 > 50ms for 2m"
  topOffenders:
    - namespace: data-pipeline
      pod: spark-executor-7f8b4
      container: spark
      confidence: 0.85
      ownerKind: Deployment
      metrics:
        cpuRunQueueImpact: 67ms
        ioThroughputBytes: 524288000
    - namespace: batch-jobs
      pod: etl-worker-2c9f1
      container: etl
      confidence: 0.62
      ownerKind: Job
      metrics:
        ioThroughputBytes: 314572800
  victimPods:
    - namespace: api-serving
      pod: web-frontend-abc12
      degradation:
        cpuRunQueueLatencyP99: 55ms  # normally 5ms
  contentionTypes:
    - cpu
    - io
  numaContention:           # only present if NUMA observer enabled
    node0: Healthy
    node1: Critical
  history:
    - timestamp: "2026-04-07T10:14:00Z"
      action: Taint
      severity: Critical
      offenders:
        - data-pipeline/spark-executor-7f8b4 (confidence: 0.85)
      reason: "CPU run queue latency p99 62ms (threshold: 50ms)"
```

### 6.6 Decision Engine `[Core]`

The decision engine runs in the controller and evaluates contention reports against NodeHealthPolicy. The logic follows a tiered escalation model:

**Visual flow:**

```
  ContentionReports ──► COLLECT ──► VALIDATE ──► FILTER (noise) ──► AGGREGATE
                                                                        │
                          ┌─────────────────────────────────────────────┘
                          ▼
                      ATTRIBUTE (top-N offenders + confidence)
                          │
                          ▼
                      CLASSIFY (Healthy / Warning / Critical / Emergency)
                          │
                          ▼
                   ┌──────┴──────┐
                   │ CONFIDENCE  │ confidence < threshold?
                   │   GATE      │──── YES ──► alert only
                   └──────┬──────┘
                          │ NO
                          ▼
                   ┌──────┴──────┐
                   │ MODE CHECK  │ observe / alert / enforce?
                   └──────┬──────┘
                          │ enforce
                          ▼
                   DEBOUNCE ──► COOLDOWN ──► SAFETY CHECKS ──► EXECUTE
                                                                  │
                                                                  ▼
                                                    ANNOTATE + RECORD + EVENT
```

**Concrete example of the pipeline in action:**

```
Input:  CPU run queue latency p99 = 65ms across 12 samples in 2m window
        → 65ms > 50ms critical threshold → severity: CRITICAL
        → spark-executor-7f8b4 intensity: 0.78, fair share: 0.25, excess: 0.53
        → victim web-frontend latency degraded 11x from baseline
        → temporal correlation: 0.82 (strong)
        → confidence: 0.85

Result: confidence 0.85 >= taint threshold 0.7 → taint permitted
        confidence 0.85 < evict threshold 0.9 → eviction NOT permitted
        → Action: apply NoSchedule taint, emit event, annotate node
```

**Detailed pipeline:**

```
For each node, every evaluation cycle:

1.  COLLECT: Gather all ContentionReports within the evaluation window
2.  VALIDATE: Ensure minSamples are present (avoid acting on sparse data)
3.  FILTER: Apply burst filter — discard contention lasting < burstFilterWindow
4.  AGGREGATE: Compute per-metric percentiles across the window
5.  ATTRIBUTE: Identify top-N offending pods (Section 7.5)
    - Compute per-offender confidence scores
    - Handle multi-offender scenarios (combined intensity)
6.  CLASSIFY: Map aggregated metrics to severity tier
    - All below warning thresholds → Healthy
    - Any above warning, none above critical → Warning
    - Any above critical, none above emergency → Critical
    - Any above emergency → Emergency
7.  CONFIDENCE GATE: Check if attribution confidence permits the action
    - confidence < 0.5 → no report
    - confidence 0.5-0.7 → alert only
    - confidence 0.7-0.9 → taint/cordon allowed
    - confidence > 0.9 → eviction allowed
    (thresholds configurable via actionConfidenceOverrides)
8.  MODE CHECK: Respect policy mode
    - observe → stop here, emit metric only
    - alert → emit event + metric, no remediation
    - enforce → proceed to action
9.  DEBOUNCE: Check if severity is same or higher than previous evaluation
    - If downgrade → require deescalationReadings consecutive lower readings
    - If upgrade → act immediately
10. COOLDOWN: Check if cooldown from previous action is active
11. SAFETY: Check maxActionsPerHour budget
12. EVICTION SAFETY: If action includes eviction, run safety checks (Section 7.6)
13. EXECUTE: Apply actions defined for the severity tier
14. ANNOTATE: Update scheduler feedback annotations on the node
15. RECORD: Update NodeContentionStatus CR, emit events
```

### 6.7 Scheduler Feedback Loop `[Extended]`

The scheduler feedback loop prevents recurrence of noisy neighbor problems by influencing future pod placement.

**How it works:**

When the controller detects contention, it annotates the affected node with the type and severity of contention. The scheduler (or operators via pod specs) uses this information to avoid placing contention-sensitive workloads on already-hot nodes.

**Annotation-based integration (default, zero scheduler changes):**

```yaml
# Applied to node by controller
metadata:
  labels:
    sentinel.io/health: "degraded"          # for nodeAffinity rules
  annotations:
    sentinel.io/contention-type: "cpu,io"   # which resources are contended
    sentinel.io/contention-score: "0.8"     # 0.0 = healthy, 1.0 = fully contended
    sentinel.io/contention-since: "2026-04-07T10:14:00Z"
    sentinel.io/offender-namespaces: "data-pipeline,batch-jobs"
```

Operators can use these in pod specs:

```yaml
# Pod spec: avoid nodes with active contention
affinity:
  nodeAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 80
        preference:
          matchExpressions:
            - key: sentinel.io/health
              operator: NotIn
              values: ["degraded"]
```

**Scheduler plugin (advanced, optional):**

A custom scheduler plugin implements `ScorePlugin` from `k8s.io/scheduler-framework`:

```
Score phase:
  For each candidate node:
    1. Read sentinel.io/contention-score annotation
    2. Read pod's resource profile (cpu-intensive, io-intensive, network-intensive)
       from pod annotations: sentinel.io/workload-type
    3. If pod's workload type matches node's contention type:
       score_penalty = contention_score * 100
    4. Return: max_score - score_penalty

Result: scheduler deprioritizes nodes where the pod's workload type
would worsen existing contention.
```

**Recovery:**

When contention resolves and the controller removes taints, it also clears scheduler annotations (if `clearOnRecovery: true`). The node returns to full scheduling eligibility.

### 6.8 Deployment Topology `[Core]`

```
Namespace: sentinel-system

┌──────────────────────────────────────────────────────┐
│  DaemonSet: node-sentinel-agent                      │
│  ├── Image: node-sentinel-agent:v0.1                 │
│  ├── hostPID: true (cgroup resolution)               │
│  ├── privileged: false                               │
│  ├── capabilities: [BPF, PERFMON, SYS_RESOURCE,      │
│  │                   SYS_PTRACE]                     │
│  ├── volumeMounts:                                   │
│  │   ├── /sys/fs/bpf (BPF filesystem)                │
│  │   ├── /sys/fs/cgroup (cgroup2 tree)               │
│  │   ├── /sys/kernel/debug (tracefs)                 │
│  │   ├── /proc (host proc, read-only)                │
│  │   └── /var/run/sentinel (unix socket for CLI)     │
│  ├── resources:                                      │
│  │   ├── requests: 50m CPU, 64Mi memory              │
│  │   └── limits: 200m CPU, 128Mi memory              │
│  └── tolerations: all (must run everywhere)          │
│                                                      │
│  Deployment: node-sentinel-controller                │
│  ├── replicas: 2 (leader election)                   │
│  ├── Image: node-sentinel-controller:v0.1            │
│  ├── ServiceAccount: sentinel-controller             │
│  │   └── RBAC: nodes (get/list/watch/patch),         │
│  │     pods (get/list), pods/eviction (create),      │
│  │     events (create/patch), CRDs (full),           │
│  │     leases (get/create/update)                    │
│  └── resources:                                      │
│      ├── requests: 100m CPU, 128Mi memory            │
│      └── limits: 500m CPU, 256Mi memory              │
│                                                      │
│  Service: node-sentinel-controller                   │
│  └── ClusterIP for agent→controller gRPC             │
│                                                      │
│  ConfigMap: sentinel-agent-config                    │
│  └── agent tuning (poll intervals, etc.)             │
│                                                      │
│  ValidatingWebhookConfiguration:                     │
│  └── validates NodeHealthPolicy CRDs                 │
│      (threshold ordering, sane defaults, etc.)       │
└──────────────────────────────────────────────────────┘
```

---

## 7. Low-Level Design (LLD)

### 7.1 eBPF Programs `[Core]`

Five eBPF programs, each targeting a different contention signal. All programs are written in C (restricted C for the BPF verifier), compiled with clang/llvm to BPF bytecode, and loaded from Go using `cilium/ebpf`.

#### 7.1.1 sched_monitor.bpf.c — CPU Scheduling Contention `[Core]`

**Hook points:**
- `tp/sched/sched_switch` — fires on every context switch
- `tp/sched/sched_wakeup` — fires when a task becomes runnable

**What it measures:**
- **Run queue latency**: Time between `sched_wakeup` (task becomes runnable) and `sched_switch` (task actually gets CPU). This directly measures how long a thread waited because the CPU was busy with something else.
- **Involuntary preemptions**: Count of times a task was descheduled involuntarily.

**Data structure design:**

```c
// Key for per-cgroup latency tracking
struct sched_key {
    u64 cgroup_id;
};

// Histogram bucket for latency distribution
// Using log2 histogram: bucket[i] counts events with latency in [2^i, 2^(i+1)) ns
struct sched_hist {
    u64 slots[26];  // 0=<1ns ... 25=>33s (covers all practical ranges)
    u64 total_ns;   // total accumulated latency
    u64 count;      // total events
};

// BPF maps
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
    __uint(max_entries, 4096);       // up to 4096 cgroups per node
    __type(key, struct sched_key);
    __type(value, struct sched_hist);
} runq_latency_map SEC(".maps");

// Temporary map to track wakeup timestamps
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);      // max concurrent tasks
    __type(key, u32);                // pid
    __type(value, u64);              // wakeup timestamp
} wakeup_ts_map SEC(".maps");

// Ring buffer for threshold-breach events (pushed to userspace immediately)
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 256 * 1024); // 256KB ring buffer
} events SEC(".maps");
```

**Program flow:**

```
sched_wakeup handler:
  1. Read pid of task being woken
  2. Record timestamp in wakeup_ts_map[pid] = bpf_ktime_get_ns()

sched_switch handler:
  1. Read pid of task being scheduled IN (next)
  2. Lookup wakeup_ts_map[pid] for wakeup timestamp
  3. If found:
     a. delta = now - wakeup_ts
     b. cgroup_id = bpf_get_current_cgroup_id()
     c. bucket = log2(delta)
     d. runq_latency_map[cgroup_id].slots[bucket]++
     e. runq_latency_map[cgroup_id].total_ns += delta
     f. runq_latency_map[cgroup_id].count++
     g. Delete wakeup_ts_map[pid]
  4. If delta > KERNEL_SIDE_THRESHOLD (configurable via BPF global):
     a. Push event to ring buffer with (cgroup_id, pid, delta, timestamp)
```

**Why per-CPU hash map:**
Per-CPU maps avoid lock contention on the eBPF side. `sched_switch` fires at extremely high frequency (tens of thousands per second per CPU). A regular hash map would serialize updates across CPUs. Per-CPU means each CPU core writes to its own copy, and the Go agent merges them during reads.

#### 7.1.2 blkio_monitor.bpf.c — Block I/O Contention `[Core]`

**Hook points:**
- `tp/block/block_rq_insert` — block request submitted to device queue
- `tp/block/block_rq_complete` — block request completed

**What it measures:**
- **I/O latency per cgroup**: Time from request insert to completion, bucketed by cgroup.
- **I/O throughput per cgroup**: Bytes submitted per cgroup in each interval.
- **Queue depth contribution**: How many outstanding requests a cgroup has at any time.

**Data structure design:**

```c
struct blkio_key {
    u64 cgroup_id;
    u8  op;           // 0=read, 1=write, 2=discard
};

struct blkio_hist {
    u64 latency_slots[26];   // log2 histogram of latency (ns)
    u64 total_bytes;
    u64 total_ops;
    u64 total_latency_ns;
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct blkio_key);
    __type(value, struct blkio_hist);
} blkio_latency_map SEC(".maps");

// Track in-flight requests for latency calculation
struct rq_info {
    u64 start_ns;
    u64 cgroup_id;
    u32 bytes;
    u8  op;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 65536);
    __type(key, u64);              // request pointer as key
    __type(value, struct rq_info);
} inflight_rq SEC(".maps");
```

**Program flow:**

```
block_rq_insert handler:
  1. Capture request pointer, current cgroup_id, bytes, op type
  2. Store in inflight_rq: key=request_ptr, value={now, cgroup_id, bytes, op}

block_rq_complete handler:
  1. Lookup inflight_rq[request_ptr]
  2. If found:
     a. delta = now - start_ns
     b. Update blkio_latency_map[{cgroup_id, op}] histogram
     c. Add bytes to total_bytes
     d. Delete inflight_rq[request_ptr]
  3. If delta > threshold → push to ring buffer
```

#### 7.1.3 net_monitor.bpf.c — Network Contention `[Core]`

**Hook points:**
- `kprobe/tcp_retransmit_skb` — TCP retransmission
- `tp/net/net_dev_queue` — packet queued to device
- `tp/skb/kfree_skb` — packet dropped (with reason)

**What it measures:**
- **TCP retransmit rate per cgroup**: Direct signal that network contention is causing reliability issues.
- **Packet drop rate per cgroup**: Packets dropped at the kernel level.
- **TX queue latency**: Time packet spends waiting in the device queue.

**Data structure design:**

```c
struct net_key {
    u64 cgroup_id;
};

struct net_stats {
    u64 retransmits;
    u64 drops;
    u64 tx_packets;
    u64 tx_bytes;
    u64 rx_packets;
    u64 rx_bytes;
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_HASH);
    __uint(max_entries, 4096);
    __type(key, struct net_key);
    __type(value, struct net_stats);
} net_stats_map SEC(".maps");
```

#### 7.1.4 mm_monitor.bpf.c — Memory Pressure `[Experimental]`

**Hook points:**
- `tp/vmscan/mm_vmscan_direct_reclaim_begin` — direct reclaim triggered
- `tp/vmscan/mm_vmscan_direct_reclaim_end` — direct reclaim completed
- `tp/oom/oom_score_adj_update` — OOM score changes

**What it measures:**
- **Direct reclaim frequency per cgroup**: How often a cgroup triggers memory reclaim, which stalls all allocations on the node.
- **Direct reclaim latency**: Time spent in reclaim.

Memory pressure signals are noisier and harder to attribute to a single offender than CPU or I/O signals. The difficulty arises because direct reclaim is triggered globally — when any allocation fails to find free pages, the kernel reclaims from all eligible cgroups, not just the one that triggered the pressure. Attribution relies on correlating "which cgroup's allocation rate spiked before the reclaim event," which is inherently less precise than the direct per-cgroup measurement available for CPU and I/O.

Ships disabled by default. Operators should enable it only on nodes where memory pressure is a known issue and they accept the higher false-attribution rate.

#### 7.1.5 numa_monitor.bpf.c — NUMA Cross-Node Contention `[Extended]`

**Hook points:**
- `tp/sched/sched_switch` — augmented with CPU-to-NUMA mapping
- `tp/migrate/mm_migrate_pages` — page migration between NUMA nodes

**What it measures:**
- **Per-NUMA-node scheduling latency**: Run queue latency partitioned by NUMA node, revealing whether contention is localized to one socket.
- **Cross-NUMA page migration rate per cgroup**: How often a cgroup's memory is migrated between NUMA nodes (indicates poor locality, potential contention source).

See Section 7.10 for full design.

### 7.2 Go Agent (node-sentinel-agent) `[Core]`

#### 7.2.1 Package Structure

```
cmd/
  agent/
    main.go                    # entry point, flag parsing, signal handling
  sentinelctl/
    main.go                    # CLI entry point
internal/
  agent/
    agent.go                   # top-level agent lifecycle (start, stop, reload)
    config.go                  # agent configuration
  ebpf/
    loader.go                  # loads compiled BPF objects, attaches programs
    sched.go                   # reads sched_monitor maps, computes histograms
    blkio.go                   # reads blkio_monitor maps
    net.go                     # reads net_monitor maps
    mm.go                      # reads mm_monitor maps
    numa.go                    # reads numa_monitor maps
    ringbuf.go                 # ring buffer consumer (shared across programs)
    types.go                   # Go representations of BPF map keys/values
    observer.go                # Observer interface (for plugin model)
  cgroup/
    resolver.go                # cgroup_id → (namespace, pod, container) mapping
    watcher.go                 # inotify/fsnotify on cgroup tree for live updates
  numa/
    topology.go                # NUMA topology discovery via /sys/devices/system/node
    mapper.go                  # CPU → NUMA node mapping
  metrics/
    aggregator.go              # aggregates raw map data into ContentionReport
    histogram.go               # percentile computation from log2 histograms
    baseline.go                # rolling baseline tracker (EMA)
    smoothing.go               # burst filtering and signal smoothing
  reporter/
    grpc_client.go             # gRPC stream to controller
    standalone.go              # standalone mode (log + metrics only)
  server/
    prometheus.go              # /metrics endpoint
    health.go                  # /healthz, /readyz endpoints
    local_grpc.go              # unix socket gRPC for sentinelctl
```

#### 7.2.2 Agent Main Loop

```
Agent.Start():
  1. Load BPF objects (compiled .o files embedded in binary via go:embed)
  2. Attach BPF programs to tracepoints/kprobes
  3. Initialize NUMA topology mapper (read /sys/devices/system/node/)
  4. Initialize cgroup resolver (scan /sys/fs/cgroup, build initial map)
  5. Start cgroup watcher (detect new/deleted cgroups → new/deleted pods)
  6. Start ring buffer consumer goroutine
  7. Start periodic map reader goroutine (configurable interval, default 5s)
  8. Start gRPC reporter goroutine
  9. Start Prometheus metrics server
  10. Start local gRPC server (unix socket) for sentinelctl

Map Reader Goroutine (every 5s):
  1. For each enabled observer (BPF program):
     a. Iterate the per-CPU hash map
     b. Merge per-CPU values (sum histograms, sum counters)
     c. If NUMA enabled: partition merged data by NUMA node
     d. Resolve cgroup_id → pod identity
     e. Compute percentiles from merged histogram
     f. Apply EMA smoothing against rolling baseline
     g. Apply burst filter (discard spikes shorter than burstFilterWindow)
     h. Mark as anomalous if smoothed value > baseline + deviationSigma * stddev
  2. Build ContentionReport protobuf
  3. Send to reporter channel
  4. Reset maps (read-and-delete pattern — see 6.2.3)

Ring Buffer Consumer Goroutine:
  1. Block on ring buffer read (epoll-based via cilium/ebpf)
  2. On event:
     a. Parse event struct
     b. Resolve cgroup_id → pod identity
     c. Emit immediate Prometheus metric
     d. Forward to reporter as urgent event
```

#### 7.2.3 Map Reset Strategy

eBPF maps accumulate data continuously. The agent needs point-in-time snapshots for each reporting interval. Two options:

**Option A: Read-and-Delete** (chosen)
- For each key in the map, read the value, then delete the key.
- Next interval starts from zero.
- Pro: Clean separation between intervals, no double-counting.
- Con: Small window where an in-kernel update to a key might happen between read and delete. Acceptable — the dropped event will appear in the next interval.

**Option B: Double Buffering**
- Maintain two maps, swap the active map each interval using a BPF global variable.
- Pro: Zero data loss.
- Con: More complex BPF code, doubles memory usage, adds a branch to every BPF program.

Option A is simpler and sufficient for our reporting intervals (5-10s). The occasional dropped event is negligible.

### 7.3 Controller (node-sentinel-controller) `[Core]`

#### 7.3.1 Package Structure

```
cmd/
  controller/
    main.go
internal/
  controller/
    controller.go             # top-level controller lifecycle
    leader.go                 # leader election via controller-runtime
  policy/
    evaluator.go              # evaluates ContentionReports against NodeHealthPolicy
    threshold.go              # threshold comparison logic
    debounce.go               # severity debouncing (avoid flapping)
    resolver.go               # multi-policy conflict resolution (priority-based)
    validator.go              # webhook validation logic
  attribution/
    engine.go                 # top-N offender identification
    confidence.go             # confidence scoring
    multi_offender.go         # combined intensity analysis
    baseline.go               # per-pod baseline from agent reports
  action/
    executor.go               # action dispatcher
    taint.go                  # apply/remove taints
    cordon.go                 # cordon/uncordon nodes
    evict.go                  # evict pods (via eviction API)
    event.go                  # emit K8s events
    safety.go                 # eviction safety checks
    scheduler.go              # node annotation for scheduler feedback
  state/
    node_state.go             # in-memory state per node (current severity, history)
    window.go                 # sliding window implementation
  grpc/
    server.go                 # gRPC server receiving agent reports
    proto/
      sentinel.proto          # protobuf definitions
  reconciler/
    policy_reconciler.go      # watches NodeHealthPolicy CRDs
    status_reconciler.go      # updates NodeContentionStatus CRs
    recovery_reconciler.go    # periodic recovery check for tainted/cordoned nodes
  webhook/
    validator.go              # validating admission webhook for CRDs
```

#### 7.3.2 Controller Main Loop

```
Controller.Start():
  1. Initialize controller-runtime manager with leader election
  2. Register reconcilers:
     a. PolicyReconciler — watches NodeHealthPolicy, caches active policies
     b. StatusReconciler — updates NodeContentionStatus every evaluation cycle
     c. RecoveryReconciler — runs every recoveryCheckInterval
  3. Register validating webhook for NodeHealthPolicy
  4. Start gRPC server for agent reports
  5. Start evaluation ticker (every evaluation.interval)

Evaluation Cycle (every 10s):
  For each node with recent reports:
    1. Resolve applicable NodeHealthPolicy (priority-based if multiple match)
    2. Check policy mode (observe/alert/enforce)
    3. Collect all ContentionReports in the evaluation window
    4. If reports.count < minSamples → skip (insufficient data)
    5. Aggregate metrics across reports (p50, p95, p99 per metric)
    6. Run attribution algorithm (Section 7.5) — identify top-N offenders
    7. Classify severity: Healthy / Warning / Critical / Emergency
    8. Gate on confidence: check per-action confidence requirements
    9. Apply debounce logic
    10. Check cooldown for this node
    11. Check maxActionsPerHour budget
    12. If action includes eviction → run eviction safety checks (Section 7.6)
    13. Execute permitted actions
    14. Update scheduler feedback annotations
    15. Update NodeContentionStatus CR
    16. Emit K8s event

RecoveryReconciler (every recoveryCheckInterval):
  For each node with active sentinel actions:
    1. Check if contention metrics have returned below warning thresholds
    2. Check deescalationReadings consecutive healthy readings met
    3. If autoRecover enabled and contention resolved:
       a. Remove taint
       b. Uncordon
       c. Clear scheduler feedback annotations (if clearOnRecovery)
       d. Update NodeContentionStatus
       e. Emit recovery event
```

### 7.4 Cgroup-to-Pod Resolution `[Core]`

This is the glue between kernel-level data (cgroup IDs) and Kubernetes-level identities (namespace/pod/container). It's fiddly but essential.

**cgroups v2 hierarchy for Kubernetes pods:**

```
/sys/fs/cgroup/
  kubepods.slice/
    kubepods-burstable.slice/
      kubepods-burstable-pod<UID>.slice/
        cri-containerd-<CONTAINER_ID>.scope    ← this is what we see in eBPF
```

**Handling nested cgroups and runtime variations:**

Different container runtimes and systemd versions create varying cgroup hierarchy depths. The resolver does not assume a fixed depth. Instead it:

1. Walks the cgroup tree bottom-up from each leaf
2. Matches path segments against known patterns:
   - `cri-containerd-<ID>.scope` → containerd
   - `crio-<ID>.scope` → CRI-O
   - `docker-<ID>.scope` → dockerd (legacy)
3. Extracts the container ID from the matching segment
4. Walks up to find the `pod<UID>` segment for the pod UID
5. Falls back to CRI API query if path parsing fails

**Resolution strategy:**

```
1. On agent startup:
   a. Walk /sys/fs/cgroup recursively
   b. For each leaf cgroup (container scope):
      - Read the cgroup inode number → this is the cgroup_id we see in eBPF
      - Parse the path to extract pod UID and container ID
      - Query CRI (containerd/CRI-O) gRPC API to map container ID → pod metadata
      - Also fetch ownerKind (Deployment, StatefulSet, DaemonSet, Job)
   c. Build in-memory map: cgroup_id → {namespace, pod, container, qos_class, ownerKind}

2. Ongoing updates:
   a. Use inotify/fsnotify on /sys/fs/cgroup/kubepods.slice/ for create/delete events
   b. On new cgroup: resolve and add to map
   c. On deleted cgroup: remove from map
   d. Periodic full rescan every 60s as safety net (catches missed inotify events)

3. Fallback for unresolvable cgroup IDs:
   a. Label as "unknown" in metrics
   b. Do not flag unknown cgroups as offenders (could be system processes)
   c. Log at debug level for troubleshooting
```

**CRI compatibility:**
- containerd: gRPC at `unix:///run/containerd/containerd.sock`
- CRI-O: gRPC at `unix:///var/run/crio/crio.sock`
- Agent auto-detects by trying both, or operator configures via flag.

### 7.5 Anomaly Detection and Attribution Algorithm `[Core]`

The core intellectual problem: given that node-wide contention is detected, which pod(s) are responsible?

**Important design principle: Attribution is probabilistic, not deterministic.**

The system cannot prove causation — it identifies statistical correlation between a pod's resource intensity and other pods' performance degradation. This is an inherent limitation of observing shared resources: when Pod A and Pod B both spike I/O simultaneously and Pod C suffers, we can identify that A and B are likely responsible, but we cannot prove which one's I/O requests were ahead of C's in the queue. The design acknowledges this and uses confidence scoring to gate actions accordingly.

**Operational implication: When attribution is uncertain, the system always prefers alerting over automated action.** This means some genuine noisy neighbors will go unremediated when the signal is ambiguous. This is the correct trade-off — a false eviction of a critical pod is strictly worse than a delayed detection of a noisy neighbor. Operators can always investigate alerts manually and take action themselves.

**Real-world ambiguity cases where attribution fails gracefully:**
- **Simultaneous batch jobs**: Three ETL jobs start at the same time, each contributing 30% of I/O. No single offender → confidence capped at 0.5 → alert only.
- **System process contention**: A kernel worker thread (kswapd, jbd2) causes latency spikes. The cgroup resolver labels it "unknown" → never flagged as offender → node-level alert only.
- **Cascading contention**: Pod A's CPU usage causes Pod B's I/O to queue up (Pod B's threads can't submit I/O fast enough). The attribution engine sees Pod A as CPU offender and Pod B as I/O victim, but if Pod B's queued I/O also hurts Pod C, the chain is correctly tracked since Pod A is the root cause from the CPU perspective.

**Step 1: Detect node-level contention**

Contention exists when the aggregate (cross-pod) p99 of any observed metric exceeds the warning threshold. If no contention exists at the node level, we don't look for offenders.

**Step 2: Compute per-pod resource usage intensity**

For each pod on the node, compute a "resource intensity score" per metric:

```
For CPU:
  intensity(pod) = sum(cpu_time_consumed_by_pod) / sum(cpu_time_consumed_all_pods)

For Block I/O:
  intensity(pod) = sum(io_bytes_pod) / sum(io_bytes_all_pods)
  io_queue_share(pod) = sum(io_ops_pod) / sum(io_ops_all_pods)

For Network:
  intensity(pod) = sum(tx_bytes_pod + rx_bytes_pod) / sum(tx_bytes_all + rx_bytes_all)

Fair share for pod = pod_resource_request / sum(all_pod_resource_requests)
  (falls back to 1/N if no requests are set)
```

**Step 3: Compute per-pod victim impact**

For each non-offender pod, compute how much its latency metrics have degraded compared to its rolling baseline:

```
degradation(victim_pod, metric) = current_p99(metric) / baseline_p99(metric)
```

A pod is a "victim" if its degradation ratio exceeds a configurable threshold (default: 2.0x baseline).

**Step 4: Multi-offender identification**

Unlike v0.1 which assumed a single offender, the system now identifies **top-N offenders** ranked by their contribution to contention:

```
For each pod where intensity > fair_share:
  excess_intensity = intensity(pod) - fair_share(pod)
  offender_score = excess_intensity * correlation_with_victim_degradation

Sort pods by offender_score descending.
Report top-N (configurable, default 3).

Combined intensity check:
  If sum(top-N excess_intensities) < 0.6 * total_intensity:
    → contention is diffuse, no clear offenders
    → confidence is capped at 0.5 (alert only)
```

This handles the "3 pods together cause contention but none individually looks extreme" case.

**Step 5: Temporal correlation**

Within the evaluation window, check whether offender intensity and victim degradation move together across reporting intervals:

```
For each candidate offender:
  Compute Pearson correlation between:
    - offender's intensity time series (per interval)
    - maximum victim degradation time series (per interval)
  
  correlation > 0.7 → strong signal, boost confidence
  correlation 0.4-0.7 → moderate signal, neutral
  correlation < 0.4 → weak signal, reduce confidence
```

**Step 6: Confidence scoring**

```
For each offender:
  base_confidence = min(
    excess_intensity / fair_share,         # how much more than fair share
    max_victim_degradation_ratio / 2.0,    # how badly victims are affected (normalized)
    samples_in_window / minSamples         # data sufficiency
  )
  
  # Adjust for temporal correlation
  if temporal_correlation > 0.7:
    confidence = base_confidence * 1.2
  elif temporal_correlation < 0.4:
    confidence = base_confidence * 0.6
  
  # Cap for diffuse contention
  if combined_top_N_intensity < 0.6 * total:
    confidence = min(confidence, 0.5)
  
  confidence = clamp(confidence, 0.0, 1.0)

Action gating:
  confidence < 0.5               → not reported
  confidence 0.5 - actionConfidenceOverrides.taint  → alert only
  confidence >= actionConfidenceOverrides.taint      → taint permitted
  confidence >= actionConfidenceOverrides.cordon     → cordon permitted
  confidence >= actionConfidenceOverrides.evict      → eviction permitted
```

### 7.6 Eviction Safety Model `[Core]`

Eviction is the most dangerous automated action. It must be gated behind multiple safety checks.

**Pre-eviction checklist (all must pass):**

```
1. CONFIDENCE CHECK
   └── Is attribution confidence >= eviction threshold (default 0.9)?
       NO → fall back to cordon only

2. WORKLOAD TYPE CHECK
   └── Is the offender's ownerKind in the allow list?
       - Deployment, ReplicaSet, Job → allowed (by default)
       - StatefulSet → DENIED (data loss risk)
       - DaemonSet → DENIED (restarts on same node, pointless)
       - Bare pod (no owner) → DENIED (no controller to recreate)
       NO → fall back to cordon only

3. NAMESPACE CHECK
   └── Is the offender in a denied namespace?
       - kube-system → DENIED
       - sentinel-system → DENIED
       YES → fall back to cordon only

4. LABEL CHECK
   └── Does the offender have sentinel.io/critical=true?
       YES → fall back to cordon only
   └── Does the offender have sentinel.io/evictable=true? (if allow requires it)
       NO → fall back to cordon only

5. PDB CHECK
   └── Would evicting this pod violate a PodDisruptionBudget?
       YES → fall back to cordon only

6. REPLICA CHECK
   └── Does the offender's controller have other ready replicas?
       NO → log warning, proceed only if policy explicitly allows

7. RATE LIMIT CHECK
   └── Have we already evicted a pod on this node this hour?
       YES → fall back to cordon only (maxActionsPerHour)
```

**If eviction is denied at any step:**

Execute the configured `fallbackAction` (default: Cordon). The system never silently skips remediation — it always does the safest available action and logs why eviction was denied.

**DaemonSet handling:**

DaemonSet pods are never evicted because they immediately restart on the same node. Instead, when a DaemonSet pod is identified as the offender:
1. Emit a specific event: `DaemonSetNoisyNeighbor`
2. Include the DaemonSet name in the event message
3. The operator/team that owns the DaemonSet needs to manually remediate (resource limits, scheduling constraints, etc.)

**Cluster-wide blast radius protection:**

Per-node safety limits (`maxActionsPerHour`) prevent sentinel from thrashing a single node. But a correlated event — say, a noisy DaemonSet update rolling across 50 nodes simultaneously — could trigger actions on many nodes at once, effectively reducing cluster capacity.

The `clusterSafety` config in the CRD prevents this:

- `maxConcurrentTaintedNodes: 10%` — if 10% of worker nodes are already tainted by sentinel, new taints are deferred (queued, not dropped). The controller evaluates the highest-severity nodes first.
- `maxConcurrentCordonedNodes: 5%` — cordon is more disruptive than taint (fully blocks scheduling), so the cap is tighter.
- `maxEvictionsPerHour: 10` — cluster-wide eviction rate limit, independent of per-node limits.

When a cluster-wide limit is hit, the controller emits a `ClusterSafetyLimitReached` event and a `sentinel_cluster_safety_throttled` metric. Existing actions remain — only new actions on additional nodes are deferred until headroom opens up (via recovery on previously tainted nodes).

### 7.7 Taint/Cordon Lifecycle `[Core]`

```
                    ┌─────────┐
                    │ Healthy │ ◄────────────────────────────┐
                    └────┬────┘                              │
                         │ threshold breached                 │ deescalationReadings
                         ▼                                   │ consecutive healthy
                    ┌─────────┐                              │
                    │ Warning │                              │
                    │         │ event + metric +              │
                    │         │ scheduler annotation          │
                    └────┬────┘                              │
                         │ sustained > window                 │
                         ▼                                   │
                    ┌──────────┐                             │
             ┌──────│ Critical │                             │
             │      │          │ taint NoSchedule +           │
             │      │          │ scheduler annotation         │
             │      └────┬─────┘                             │
             │           │ sustained > 2x window              │
             │           ▼                                   │
             │      ┌───────────┐                            │
             │      │ Emergency │                            │
             │      │           │ cordon + evict (if safe)    │
             │      │           │ OR cordon only (fallback)   │
             │      └─────┬─────┘                            │
             │            │                                  │
             │            ▼                                  │
             │      ┌───────────┐                            │
             └─────►│ Cooldown  │────── timer expires ───────┘
                    │           │    re-evaluate
                    └───────────┘
```

**Node annotations managed by sentinel:**

```yaml
metadata:
  labels:
    sentinel.io/health: "degraded"          # for scheduling
  annotations:
    sentinel.io/severity: "critical"
    sentinel.io/contention-type: "cpu,io"
    sentinel.io/contention-score: "0.8"
    sentinel.io/contention-since: "2026-04-07T10:14:00Z"
    sentinel.io/last-action: "taint"
    sentinel.io/last-action-time: "2026-04-07T10:14:00Z"
    sentinel.io/offenders: "data-pipeline/spark-executor-7f8b4(0.85),batch-jobs/etl-worker-2c9f1(0.62)"
    sentinel.io/cooldown-until: "2026-04-07T10:19:00Z"
```

### 7.8 eBPF Map Design Summary `[Core]`

| Map | Type | Key | Value | Max Entries | Purpose |
|-----|------|-----|-------|-------------|---------|
| `runq_latency_map` | PERCPU_HASH | cgroup_id | sched_hist (histogram + counters) | 4096 | CPU scheduling latency per cgroup |
| `wakeup_ts_map` | HASH | pid (u32) | timestamp (u64) | 65536 | Track task wakeup time for latency calc |
| `blkio_latency_map` | PERCPU_HASH | {cgroup_id, op} | blkio_hist | 4096 | Block I/O latency + throughput per cgroup |
| `inflight_rq` | HASH | request_ptr (u64) | rq_info | 65536 | Track in-flight block requests |
| `net_stats_map` | PERCPU_HASH | cgroup_id | net_stats | 4096 | Network counters per cgroup |
| `mm_reclaim_map` | PERCPU_HASH | cgroup_id | reclaim_hist | 4096 | Memory reclaim latency + frequency |
| `numa_sched_map` | PERCPU_HASH | {cgroup_id, numa_node} | sched_hist | 8192 | Per-NUMA scheduling latency |
| `numa_migrate_map` | PERCPU_HASH | cgroup_id | migrate_stats | 4096 | Page migration counters |
| `events` | RINGBUF | — | event struct | 256KB | Urgent threshold-breach events to userspace |
| `config` | ARRAY | index (u32) | config_val (u64) | 16 | Runtime-configurable thresholds from userspace |

**Map sizing rationale:**
- 4096 entries for per-cgroup maps: supports up to 4096 active containers per node. Most nodes run 30-250 pods. This gives ~16x headroom.
- 8192 for NUMA-partitioned maps: 4096 cgroups × 2 NUMA nodes (typical dual-socket). Scales to 4-socket with 16384.
- 65536 for per-PID/per-request maps: supports maximum concurrent tasks/requests. Linux default max PID is 32768; 65536 gives 2x headroom.
- 256KB ring buffer: at ~64 bytes per event and a consumer reading within 100ms, this handles ~4000 events per consumer cycle. Sufficient for burst scenarios.

### 7.9 Ring Buffer Protocol `[Core]`

The ring buffer carries urgent events from kernel to userspace. These are events that should be processed immediately rather than waiting for the next map read cycle.

**Event structure (shared between C and Go):**

```c
struct sentinel_event {
    u64 timestamp_ns;      // bpf_ktime_get_ns()
    u64 cgroup_id;
    u32 pid;
    u32 event_type;        // enum: SCHED_LATENCY, IO_LATENCY, NET_RETRANSMIT,
                           //       MM_RECLAIM, NUMA_MIGRATE
    u64 value;             // the measured value (latency ns, bytes, count)
    u64 threshold;         // the threshold that was breached
    u8  numa_node;         // NUMA node where event occurred (0xFF if N/A)
    u8  reserved[7];       // alignment padding
};
```

**Go consumer:**

```go
reader, err := ringbuf.NewReader(objs.Events)
// ...
for {
    record, err := reader.Read()  // blocks via epoll
    // parse record.RawSample into sentinelEvent
    // forward to metrics + reporter
}
```

The ring buffer is shared across all eBPF programs (single `events` map pinned to BPF filesystem and referenced by all programs). This simplifies the Go consumer — one goroutine handles all urgent events.

### 7.10 NUMA-Aware Contention Detection `[Extended]`

On multi-socket bare-metal servers, contention can be localized to a single NUMA node. A pod whose threads are scheduled on NUMA node 0 may be starving other pods on node 0 while pods on node 1 are unaffected. Without NUMA awareness, the system would see aggregate contention at a moderate level instead of severe contention on one node and none on the other.

**How it works:**

1. **Topology discovery** (agent startup):
   - Read `/sys/devices/system/node/` to discover NUMA nodes
   - Build CPU → NUMA node mapping from `/sys/devices/system/node/nodeN/cpulist`
   - Store as in-memory lookup table: cpu_id → numa_node_id

2. **eBPF-side NUMA attribution**:
   - In `sched_switch` handler, read current CPU via `bpf_get_smp_processor_id()`
   - Look up NUMA node from a BPF ARRAY map pre-populated by the agent at startup
   - Write to `numa_sched_map` with compound key: `{cgroup_id, numa_node}`

3. **Agent-side aggregation**:
   - Read `numa_sched_map` and partition metrics by NUMA node
   - Report both aggregate (node-wide) and per-NUMA metrics to controller

4. **Controller-side evaluation**:
   - Evaluate thresholds both at aggregate level and per-NUMA level
   - A node can be `Healthy` in aggregate but `Critical` on one NUMA node
   - NodeContentionStatus includes per-NUMA phase

**NUMA-specific actions:**

NUMA-localized contention doesn't necessarily warrant tainting the whole node. The controller can:
- Annotate the node with the hot NUMA node: `sentinel.io/numa-hot=0`
- If the scheduler plugin is active, it can prefer placing pods on the healthy NUMA node
- Tainting/cordoning the whole node is still the fallback for severe NUMA-localized contention

**When to enable:**
- Multi-socket bare-metal only. Cloud VMs are typically single-NUMA.
- Disabled by default (`observers.numa.enabled: false`).

### 7.11 Agent CLI — sentinelctl `[Core]`

A CLI tool for on-node debugging. Connects to the agent's local gRPC server via unix socket.

**Commands:**

```
sentinelctl top
  → Live view of per-pod contention metrics (like htop but for contention)
  → Columns: NAMESPACE, POD, CPU_RQL_P99, IO_LAT_P99, NET_RETX, INTENSITY, STATUS
  → Refreshes every 2s

sentinelctl status
  → Current node contention status (severity, active actions, offenders)

sentinelctl baseline <namespace/pod>
  → Show rolling baseline for a specific pod (what's "normal")

sentinelctl dump-maps
  → Raw dump of eBPF map contents (for debugging)

sentinelctl observers
  → List loaded eBPF programs and their attachment status

sentinelctl numa
  → Per-NUMA-node contention summary (if NUMA observer enabled)
```

This is deployed as a static binary in the agent container. Operators `kubectl exec` into the agent pod and run `sentinelctl` directly.

---

## 8. Controller State Model `[Core]`

The controller manages state at two levels. Understanding which state is in-memory and which is persisted is critical for recovery behavior.

### 8.1 Persisted State (survives controller restarts)

| State | Storage | Owner | Purpose |
|-------|---------|-------|---------|
| `NodeHealthPolicy` CRs | etcd (via CRD) | Operator | Policy definitions — thresholds, actions, safety config |
| `NodeContentionStatus` CRs | etcd (via CRD) | Controller | Per-node severity, active actions, offender history |
| Node taints | etcd (via Node API) | Controller | Applied `NoSchedule` taints |
| Node annotations | etcd (via Node API) | Controller | `sentinel.io/*` annotations for scheduler + status |
| Node cordon state | etcd (via Node spec.unschedulable) | Controller | Cordon flag |

On restart, the new controller leader reconstructs its view of the world from these persisted resources. No contention data is lost — only in-flight evaluation state.

### 8.2 In-Memory State (lost on controller restart)

| State | What it holds | Impact of loss | Recovery |
|-------|---------------|----------------|----------|
| Sliding window buffer | Recent ContentionReports per node (last `evaluation.window`) | Controller cannot evaluate until agents re-fill the window | Agents continue sending. Window refills within `evaluation.window` (default 2m). Controller does NOT act during this gap — `minSamples` check prevents it. |
| Per-node severity history | Previous severity for debounce logic | De-escalation counter resets to 0 | Worst case: a node that was de-escalating resets its counter. Next evaluation re-classifies from scratch. |
| Cooldown timers | Active cooldown timestamps per node | Cooldown may fire again prematurely | Mitigated: `maxActionsPerHour` is an independent safety net. Worst case: one extra taint/untaint cycle. |
| Attribution baselines | Per-pod rolling baselines from agent reports | Baselines reset | Baselines rebuild from agent reports over `baselineWindow` (default 30m). During rebuild, anomaly detection is less precise but functional. |

**Key design decision:** The controller is designed to be restartable at any time with bounded impact. The maximum disruption from a controller restart is one `evaluation.window` (default 2m) of inaction, after which normal operation resumes. Existing taints and cordons remain in place during the gap.

---

## 9. Data Freshness and Timing Guarantees `[Core]`

Distributed timing is critical for a system that takes automated actions. These are the guarantees and their implications.

### 9.1 Freshness Guarantees

| Data path | Maximum staleness | Guarantee type |
|-----------|------------------|----------------|
| Kernel event → eBPF map | 0 (synchronous) | Hard — BPF programs execute inline with kernel events |
| eBPF map → agent report | `evaluation.interval` (default 10s) | Soft — agent reads maps on a timer, jitter ±1s |
| Agent report → controller | Network RTT + gRPC buffering (~50ms typical) | Best-effort — depends on network |
| Controller evaluation cycle | `evaluation.interval` (default 10s) | Soft — evaluation runs on a timer |
| End-to-end: kernel event → action | `evaluation.interval` × 2 + network (~20-25s typical) | Soft — sum of pipeline stages |

### 9.2 Staleness Handling

- **Agent reports carry a monotonic timestamp.** The controller uses this to detect stale reports. Reports older than 2× `evaluation.window` are discarded.
- **If a node has no reports within 2× `evaluation.interval`**, the controller marks it as `DataGap` and does NOT take any new actions. Existing taints/cordons remain.
- **If the controller falls behind**, it drops the oldest reports first (FIFO eviction from the sliding window). The `sentinel_evaluation_backpressure` metric fires.

### 9.3 Clock Handling

Agents send both wall-clock (for display) and monotonic timestamps (for ordering). The controller never compares wall-clock timestamps across agents — only monotonic timestamps within a single agent's report stream. This makes the system immune to NTP drift between nodes.

---

## 10. Scaling `[Core]`

### 10.1 Agent Scaling (Per-Node)

The agent scales linearly with the number of pods on a node, not with the number of events. This is because:
- eBPF programs aggregate in-kernel (histograms, not raw events)
- Map reads are O(number of cgroups), not O(number of events)
- At 250 pods/node: ~250 hash map entries to read per observer per interval

**Bottleneck analysis:**
- Map iteration: ~250 entries × 4 observers × 5s interval = 1000 lookups/5s → negligible
- Cgroup resolution: cached in-memory, O(1) per lookup after initial scan
- Ring buffer: bounded at 256KB regardless of event rate; excess events dropped (maps are authoritative)

### 10.2 Controller Scaling

The controller receives gRPC reports from all agents. At scale:

**Single controller capacity:**
- Each agent sends one ContentionReport per interval (every 10s by default)
- Report size: ~2KB per node (top pods + metrics)
- At 100 nodes: 100 reports × 2KB × 6/min = 1.2 MB/min → trivial
- At 1000 nodes: 12 MB/min → still manageable for a single controller
- At 5000 nodes: 60 MB/min → approaching single-process limits

**Scaling strategy for large clusters (1000+ nodes):**

1. **gRPC connection pooling**: Multiple gRPC streams per controller instance, load-balanced across replicas.

2. **Sharded evaluation**: Partition nodes across controller replicas by consistent hashing on node name. Each replica evaluates a subset of nodes. Leader election is per-shard, not global.

```
# Sharding model
shard_count = controller_replicas
shard_id(node) = hash(node_name) % shard_count

Controller replica 0 → evaluates nodes where shard_id = 0
Controller replica 1 → evaluates nodes where shard_id = 1
...
```

3. **Label-based partitioning**: For heterogeneous clusters, partition by node labels (zone, pool, hardware class). Each controller instance watches nodes matching its partition label.

4. **Backpressure**: If the controller falls behind on evaluations, it drops the oldest reports (staleness > 2x evaluation window) and emits a `sentinel_evaluation_backpressure` metric.

**When to shard:**
- Under 500 nodes: single controller is sufficient
- 500-2000 nodes: 2-3 controller replicas with sharding
- 2000+ nodes: label-based partitioning by zone/pool

### 10.3 gRPC Design for Scale

```
service SentinelController {
  // Bidirectional stream: agent pushes reports, controller pushes config updates
  rpc ReportStream(stream ContentionReport) returns (stream AgentDirective);
}

// AgentDirective allows the controller to:
// - Dynamically adjust reporting interval
// - Enable/disable specific observers
// - Request immediate report (for incident investigation)
```

**Connection handling:**
- Agents maintain persistent gRPC streams with reconnect on failure
- Controller uses `grpc.MaxConcurrentStreams` to cap per-connection streams
- Keepalive probes every 30s to detect dead connections
- Agent buffers up to 10 reports during disconnection (2x evaluation window at 10s interval)

---

## 11. Kernel Compatibility `[Core]`

| Feature | Minimum Kernel | Notes |
|---------|---------------|-------|
| BPF ring buffer | 5.8 | Required for event streaming |
| Per-CPU hash maps | 4.6 | Available on all supported kernels |
| bpf_get_current_cgroup_id() | 4.18 | Essential for cgroup attribution |
| cgroups v2 | 5.2 (full) | Required — cgroups v1 not supported |
| BPF trampoline (fentry/fexit) | 5.5 | Better performance than kprobes, optional |
| BPF CO-RE (Compile Once Run Everywhere) | 5.5+ with BTF | Required for portability across kernel versions |
| BPF global variables | 5.5 | Used for runtime config from userspace |

**Minimum supported kernel: 5.10** (aligns with all major LTS kernels and managed K8s platforms)

**CO-RE strategy:**
- Compile eBPF programs with CO-RE relocations using libbpf
- Embed compiled `.o` files in the Go binary via `go:embed`
- At load time, `cilium/ebpf` uses BTF info from the running kernel to relocate struct field offsets
- This means one binary works across 5.10, 5.15, 6.1, 6.6, etc. without recompilation

**Platform compatibility:**

| Platform | Kernel | cgroups v2 | BTF | Status |
|----------|--------|------------|-----|--------|
| Ubuntu 22.04 (on-prem) | 5.15 | yes | yes | Fully supported |
| Ubuntu 24.04 (on-prem) | 6.8 | yes | yes | Fully supported |
| Amazon Linux 2023 (EKS) | 6.1 | yes | yes | Fully supported |
| GKE (Container-Optimized OS) | 5.15+ | yes | yes | Fully supported |
| AKS (Ubuntu 22.04) | 5.15 | yes | yes | Fully supported |
| RHEL 9 / Rocky 9 | 5.14 | yes | yes | Fully supported |
| RHEL 8 / Rocky 8 | 4.18 | partial | limited | Not supported (cgroups v1 default) |

**Pre-flight check:**
The agent runs a kernel capability check at startup. If required features are missing (no cgroups v2, no BTF, kernel < 5.10), the agent exits with a clear error message specifying what's missing and what kernel version is required.

---

## 12. Failure Modes and Recovery `[Core]`

### 12.1 Agent Failures

| Failure | Severity | Impact | Detection | Recovery |
|---------|----------|--------|-----------|----------|
| eBPF program fails to load (verifier rejection) | **Medium** | No data collection for that observer | Agent logs error, health endpoint reports degraded | Agent continues with remaining observers. Operator alert. |
| BPF map full (max_entries hit) | **Medium** | New cgroups not tracked | Agent monitors map size via bpf_map_info | Agent logs warning. Increase max_entries requires restart. |
| Ring buffer overflow (consumer too slow) | **Low** | Urgent events dropped | Ring buffer reports dropped count | Agent logs warning. Events are opportunistic; map reads are authoritative. |
| Cgroup resolver can't reach CRI socket | **Medium** | cgroup_id → pod resolution fails | Unresolved cgroup count metric | Agent retries. Unresolved cgroups shown as "unknown". |
| Agent OOMKilled | **High** | No data from that node | Controller detects missing reports from node | DaemonSet restarts agent. Controller treats data gap as "insufficient data" (no action). |
| Agent → Controller gRPC disconnected | **Medium** | Reports don't reach controller | gRPC stream health check | Agent buffers last 10 reports in memory. Reconnects with exponential backoff. Reports expire after 2x evaluation window. |

### 12.2 Controller Failures

| Failure | Severity | Impact | Detection | Recovery |
|---------|----------|--------|-----------|----------|
| Controller crash | **High** | No policy evaluation or automated actions | K8s restarts pod. Leader election. | New leader replays state from NodeContentionStatus CRs (see Section 8). Agents continue collecting. |
| Leader election split | **High** | Two controllers might act simultaneously | controller-runtime fencing | Stale leader's API calls fail (resourceVersion conflict). New leader takes over. |
| Controller can't reach API server | **High** | Can't apply taints/cordons | API call errors | Exponential backoff retry. Actions queued in memory. |
| Stale NodeContentionStatus | **Low** | Operator sees outdated info | Status timestamp drift | Controller overwrites on next evaluation cycle. |
| Controller overloaded (large cluster) | **Medium** | Evaluation cycles skipped | sentinel_evaluation_backpressure metric | Drop stale reports. Scale controller replicas. Enable sharding. |
| Partial cluster visibility (some agents disconnected) | **Medium** | Blind spots on disconnected nodes | sentinel_connected_agents gauge vs expected | Controller does NOT act on nodes with no recent data. Existing taints remain until data resumes. |

### 12.3 Data Integrity

| Scenario | Handling |
|----------|----------|
| Clock skew between agent and controller | Agent sends wall-clock AND monotonic timestamps. Controller uses monotonic for ordering, wall-clock for display. |
| Agent restart mid-interval | First report after restart will have partial data. Controller's minSamples check handles this — won't act on sparse data. |
| BPF program detached unexpectedly | Agent periodically verifies program attachment via bpf link info. Re-attaches if needed. |
| Map read-and-delete race | Acceptable — at most one interval's worth of events for one cgroup may be double-counted or lost. Negligible at 5-10s intervals. |

### 12.4 Misclassification Handling `[Core]`

The system will sometimes be wrong. A pod gets blamed incorrectly, a node gets tainted unnecessarily, or a healthy pod gets evicted. This is an inherent consequence of probabilistic attribution. The design treats misclassification as an operational reality, not a bug to be eliminated.

**All automated actions are reversible:**
- Taints can be removed immediately: `kubectl taint nodes <node> sentinel.io/noisy-neighbor-`
- Cordons can be lifted: `kubectl uncordon <node>`
- Evicted pods are recreated by their controller (Deployment, ReplicaSet, Job)
- Scheduler annotations clear automatically on recovery (or manually via `kubectl annotate`)

**Operators can override at any time:**
- Mark a pod as exempt from blame: `sentinel.io/exempt: "true"` label
- Mark a pod as non-evictable: `sentinel.io/critical: "true"` label
- Switch policy to `alert` mode to stop all automated actions while investigating
- Adjust confidence thresholds upward to require stronger evidence before acting

**System-level safeguards against misclassification cascades:**
- `maxActionsPerHour` caps total automated actions per node (default: 5)
- Cooldown timers prevent rapid taint/untaint cycling
- `deescalationReadings` requires multiple consecutive healthy signals before removing protections
- The eviction path has 7 independent safety checks (Section 7.6) — even a correctly attributed offender may be protected from eviction

**Design philosophy:** The system prefers false negatives (missing a noisy neighbor) over false positives (blaming the wrong pod). When attribution is uncertain, it alerts and lets the operator decide. The confidence gating system ensures that the highest-risk action (eviction) requires the highest certainty (default: 0.9).

---

## 13. Security Model `[Core]`

### 13.1 Least Privilege

The agent requires elevated capabilities but **not** a fully privileged container:

```yaml
securityContext:
  privileged: false
  capabilities:
    add:
      - BPF           # load BPF programs
      - PERFMON        # attach to perf events / tracepoints
      - SYS_RESOURCE   # increase rlimit for BPF maps
      - SYS_PTRACE     # read /proc for cgroup resolution
    drop:
      - ALL
  readOnlyRootFilesystem: true
```

**hostPID: true** is required for cgroup-to-pod resolution (reading `/proc/<pid>/cgroup`). This is a standard requirement for node-level observability tools (same as Datadog Agent, Falco, Tetragon).

### 13.2 RBAC

**Agent ServiceAccount:** Minimal — only needs to read node info for self-identification and call CRI socket.

**Controller ServiceAccount:**
```yaml
rules:
  - apiGroups: [""]
    resources: ["nodes"]
    verbs: ["get", "list", "watch", "patch"]     # patch for taints + annotations
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/eviction"]
    verbs: ["create"]                             # for eviction
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["create", "patch"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["get", "list"]                        # check PDB before eviction
  - apiGroups: ["apps"]
    resources: ["deployments", "replicasets", "statefulsets", "daemonsets"]
    verbs: ["get", "list"]                        # check owner kind + replica count
  - apiGroups: ["sentinel.io"]
    resources: ["nodehealthpolicies"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["sentinel.io"]
    resources: ["nodecontentionstatuses"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]            # leader election
```

### 13.3 BPF Program Safety

All eBPF programs are verified by the kernel's BPF verifier before loading. The verifier guarantees:
- No unbounded loops
- No out-of-bounds memory access
- No arbitrary kernel memory reads (only allowed helpers)
- Programs terminate within bounded instruction count

This is a fundamental safety property of eBPF — a buggy BPF program cannot crash the kernel.

---

## 14. Observability `[Core]`

### 17.1 Agent Metrics (Prometheus)

```
# Per-pod contention metrics
sentinel_cpu_runqueue_latency_seconds{namespace, pod, container, quantile}
sentinel_blkio_latency_seconds{namespace, pod, container, op, quantile}
sentinel_blkio_throughput_bytes{namespace, pod, container, op}
sentinel_net_retransmits_total{namespace, pod, container}
sentinel_net_drops_total{namespace, pod, container}
sentinel_mm_reclaim_latency_seconds{namespace, pod, container, quantile}
sentinel_numa_runqueue_latency_seconds{namespace, pod, container, numa_node, quantile}

# Agent health metrics
sentinel_agent_ebpf_programs_loaded{program}           # gauge: 1=loaded, 0=failed
sentinel_agent_map_entries{map}                         # gauge: current entry count
sentinel_agent_ringbuf_events_total{type}               # counter
sentinel_agent_ringbuf_drops_total                      # counter
sentinel_agent_cgroup_resolution_errors_total            # counter
sentinel_agent_report_latency_seconds                   # histogram
sentinel_agent_map_read_duration_seconds{observer}      # histogram
sentinel_agent_baseline_pods_tracked                    # gauge
```

### 17.2 Controller Metrics (Prometheus)

```
# Decision metrics
sentinel_evaluation_total{node, result}                 # counter: healthy/warning/critical/emergency
sentinel_action_total{node, action_type}                # counter: taint/cordon/evict/event
sentinel_action_errors_total{node, action_type}         # counter
sentinel_action_denied_total{node, action_type, reason} # counter: pdb/statefulset/namespace/etc
sentinel_recovery_total{node}                           # counter
sentinel_cooldown_active{node}                          # gauge: 0 or 1

# Attribution metrics
sentinel_offender_detected_total{namespace, pod}        # counter
sentinel_attribution_confidence{node}                   # gauge: 0.0-1.0
sentinel_attribution_diffuse_total{node}                # counter: could not identify clear offender

# System health
sentinel_connected_agents                               # gauge
sentinel_reports_received_total{node}                   # counter
sentinel_evaluation_latency_seconds                     # histogram
sentinel_evaluation_backpressure{node}                  # gauge: reports dropped due to lag

# Policy
sentinel_policy_mode{policy, mode}                      # gauge: observe/alert/enforce
sentinel_policy_match_count{policy}                     # gauge: nodes matching this policy
```

### 17.3 Kubernetes Events

```
Type: Warning
Reason: NoisyNeighborDetected
Object: Node/worker-03
Message: "Top offenders: data-pipeline/spark-executor-7f8b4 (confidence: 0.85),
          batch-jobs/etl-worker-2c9f1 (confidence: 0.62).
          CPU run queue latency p99: 62ms (threshold: 50ms).
          Affected pods: api-serving/web-frontend-abc12 (latency 5ms → 55ms).
          Action: Node tainted with sentinel.io/noisy-neighbor=true:NoSchedule.
          Policy: default (mode: enforce)"
---
Type: Warning
Reason: EvictionDenied
Object: Node/worker-03
Message: "Eviction of data-pipeline/spark-executor-7f8b4 denied:
          ownerKind=StatefulSet is in deny list. Fallback action: Cordon applied."
---
Type: Normal
Reason: ContentionResolved
Object: Node/worker-03
Message: "Contention resolved after 8m. Taint removed. Node uncordoned.
          Scheduler annotations cleared."
---
Type: Warning
Reason: DaemonSetNoisyNeighbor
Object: Node/worker-05
Message: "DaemonSet kube-system/fluentd-gke identified as noisy neighbor
          (IO throughput: 450MB/s). Cannot evict DaemonSet pods.
          Manual remediation required."
```

---

## 15. Extensibility — Observer Plugin Model `[Experimental]`

The system is designed to support additional contention signals beyond the built-in observers. This allows operators or third-party developers to add custom eBPF programs for domain-specific contention detection.

### 15.1 Observer Interface

```go
// Observer defines the contract for a contention signal source.
type Observer interface {
    // Name returns the observer's unique identifier (e.g., "cpu", "blkio", "custom-gpu")
    Name() string
    
    // Load compiles and loads the eBPF program, attaches to hooks.
    // Returns an error if the kernel doesn't support the required features.
    Load(opts LoadOptions) error
    
    // Collect reads eBPF maps and returns per-cgroup metrics for this interval.
    Collect(resolver cgroup.Resolver) ([]CgroupMetrics, error)
    
    // Reset clears the eBPF maps for the next interval.
    Reset() error
    
    // Close detaches programs and cleans up maps.
    Close() error
    
    // RequiredKernelFeatures returns the minimum kernel features needed.
    RequiredKernelFeatures() []KernelFeature
}
```

### 15.2 Adding a Custom Observer

A custom observer consists of:
1. An eBPF C program (compiled to `.o`)
2. A Go struct implementing the `Observer` interface
3. Registration in the agent's observer registry

Example: a GPU contention observer that hooks into NVIDIA kernel driver tracepoints (if available).

### 15.3 Plugin Loading

Custom observers are compiled into the agent binary at build time (not dynamically loaded at runtime). This is a deliberate choice — dynamic BPF program loading at runtime introduces security and stability risks. New observers require an agent image rebuild and DaemonSet rolling update.

The tradeoff is operational simplicity and security over hot-reload flexibility. For most environments, the built-in observers (CPU, I/O, network, memory, NUMA) cover the primary contention vectors. Custom observers are for specialized hardware or kernel subsystems.

---

## 16. Performance Overhead Budget `[Core]`

**Claim: Agent consumes < 1% CPU and < 50MB RSS under normal conditions.**

### 22.1 eBPF In-Kernel Overhead

**sched_switch/sched_wakeup handlers:**
- sched_switch fires ~10,000-50,000 times/sec/CPU on a busy node
- Each handler execution: ~200ns (hash lookup + histogram update)
- Per CPU overhead: 50,000 × 200ns = 10ms/s = 1% of one CPU core
- On a 32-core node: 1% × 1/32 = 0.03% of total CPU
- This is the most expensive handler. Others (block, net) fire much less frequently.

**Per-CPU maps eliminate lock contention:**
- Without per-CPU: each sched_switch would contend on a spinlock across all CPUs
- With per-CPU: zero contention, each CPU writes to its own copy
- This is why we chose per-CPU maps despite the higher memory usage

**Block I/O handlers:**
- block_rq_insert/complete fire at ~1,000-10,000 times/sec (depends on workload)
- Per-handler: ~150ns
- Overhead: 10,000 × 150ns = 1.5ms/s → negligible

**Network handlers:**
- tcp_retransmit_skb: fires only on retransmits (hopefully rare)
- net_dev_queue: fires per packet, but handler is very lightweight (~100ns)
- At 100,000 packets/sec: 100,000 × 100ns = 10ms/s → 0.03% per core

### 22.2 Userspace (Go Agent) Overhead

**Map reading (every 5s):**
- 4 observers × ~250 map entries = 1000 entries to read
- Per entry read via `cilium/ebpf`: ~10μs (syscall overhead)
- Total: 1000 × 10μs = 10ms every 5s → 0.2% of one core

**Histogram computation:**
- 250 pods × 4 observers × percentile calc: ~100μs total → negligible

**Ring buffer consumer:**
- Blocks on epoll when idle (zero CPU)
- Wakes on events: ~1μs per event processing

**gRPC reporting:**
- One protobuf message per interval (~2KB)
- Serialization + send: ~1ms per interval → negligible

### 22.3 Memory Budget

| Component | Memory |
|-----------|--------|
| Go runtime baseline | ~15MB |
| eBPF map memory (per-CPU, all observers) | ~10MB (4096 entries × 4 maps × ~640 bytes/entry) |
| Cgroup resolver cache | ~5MB (4096 entries × ~1.2KB metadata) |
| Ring buffer | 256KB |
| gRPC buffers | ~2MB |
| Baseline tracker (EMA per pod per metric) | ~3MB |
| **Total** | **~35MB** |

50MB limit provides ~40% headroom for spikes (new pods, large burst of events).

---

## 17. Real-World Scenarios `[Core]`

### 17.1 Scenario: Spark Job Starves API Pods

```
Timeline:
  T+0:00  Spark executor pod starts on worker-03. Begins writing shuffle
          data at 500MB/s to local SSD.
  T+0:10  node-sentinel agent detects blkio I/O latency p99 rising for
          co-located pods (web-frontend, payment-service).
          web-frontend fsync latency: 2ms → 45ms.
          payment-service write latency: 1ms → 30ms.
  T+0:15  Agent reports to controller. Burst filter passes (> 15s sustained).
  T+0:20  Controller evaluates:
          - Aggregate I/O latency p99: 42ms (warning threshold: 100ms) → NOT breached yet
          - Anomaly detector flags: web-frontend degradation 22x above baseline
          - But node-level threshold not breached → stays at Healthy
  T+0:30  Spark executor ramps to 800MB/s. Queue depth saturates.
  T+0:40  Aggregate I/O latency p99: 180ms → WARNING threshold breached.
          Attribution: spark-executor intensity = 0.92, fair share = 0.33
          Confidence: 0.88.
          Action: Event emitted. Scheduler annotation applied.
  T+1:00  Sustained for full 2m window. minSamples met.
  T+2:40  Aggregate I/O latency p99: 340ms → CRITICAL threshold breached.
          Action: Taint applied (NoSchedule). No new pods scheduled on worker-03.
  T+5:00  Spark job completes. Executor pod terminates.
  T+5:10  Agent reports I/O latency normalizing.
  T+6:00  3 consecutive healthy readings → Recovery.
          Taint removed. Scheduler annotations cleared.
          Event: ContentionResolved.

Result: API pods experienced degradation for ~5 minutes. Without sentinel,
        this would have continued for the entire Spark job duration (potentially hours)
        and been discovered only when customers reported slowness.
```

### 17.2 Scenario: Multiple Batch Jobs Create Diffuse Contention

```
Timeline:
  T+0:00  Three ETL jobs start simultaneously on worker-07.
          Each writes at 150MB/s. Combined: 450MB/s.
  T+0:30  Aggregate I/O latency p99: 120ms → WARNING.
          Attribution engine:
          - etl-job-a: intensity 0.31, fair share 0.20, excess 0.11
          - etl-job-b: intensity 0.28, fair share 0.20, excess 0.08
          - etl-job-c: intensity 0.25, fair share 0.20, excess 0.05
          Combined top-3 excess: 0.24 / total intensity 0.84 = 0.29
          This is < 0.6 → diffuse contention → confidence capped at 0.5
  T+0:30  Action: Alert only (confidence too low for taint).
          Event: "Diffuse I/O contention detected. No single offender identified.
                  Top contributors: etl-job-a (31%), etl-job-b (28%), etl-job-c (25%)."
  T+2:30  Aggregate I/O latency p99: 350ms → CRITICAL.
          But confidence still capped at 0.5 → cannot taint.
          Controller escalates to node-level action: Cordon only (no offender-specific eviction).
  T+3:00  Event: "Node cordoned due to sustained diffuse I/O contention. No new pods scheduled."

Result: System correctly identified that no single pod was responsible,
        avoided blaming any individual job, and took the safest node-level action.
```

### 17.3 Scenario: DaemonSet Logging Agent Goes Rogue

```
Timeline:
  T+0:00  fluentd DaemonSet pod on worker-12 hits a log rotation bug.
          CPU usage spikes to 4 cores. Run queue latency for all other pods increases.
  T+0:30  WARNING: CPU run queue latency p99: 25ms.
          Attribution: fluentd (kube-system) intensity: 0.78, confidence: 0.91
  T+2:00  CRITICAL: CPU run queue latency p99: 65ms.
          Action evaluation:
          - Taint: confidence 0.91 >= 0.7 → permitted
          - Eviction: ownerKind=DaemonSet → DENIED
          Action: Taint applied. Event: DaemonSetNoisyNeighbor emitted.
          "DaemonSet kube-system/fluentd identified as noisy neighbor.
           Cannot evict DaemonSet pods — they restart on the same node.
           Manual remediation required: check fluentd resource limits."
  T+2:00  Operator receives alert, investigates, fixes fluentd config.
  T+3:00  Contention resolves. Recovery.

Result: System correctly identified the DaemonSet as offender, did not
        waste time evicting it (would restart immediately), and gave the
        operator actionable information.
```

---

## 18. Known Limitations `[Core]`

These are inherent limitations of the system's approach, not bugs to be fixed:

1. **Attribution is probabilistic, not deterministic.** The system identifies statistical correlation between resource usage and victim degradation. It cannot prove causation. In ambiguous cases, it alerts rather than acts.

2. **CPU cache contention is not directly observable.** Last-level cache (LLC) contention between pods is a real phenomenon on bare-metal, but there's no kernel tracepoint for "Pod A evicted Pod B's cache lines." We detect the downstream effect (increased memory access latency manifesting as higher run queue time) but cannot attribute it to cache specifically. Intel RDT/CMT could help but requires hardware support and kernel config that most clusters don't have.

3. **Memory pressure attribution is weak.** When the kernel enters direct reclaim, it reclaims from all eligible cgroups. Attributing "who caused the reclaim" is imprecise. The memory observer ships disabled by default for this reason.

4. **Short bursts below burstFilterWindow are invisible.** A 10-second I/O spike that causes a brief latency blip will be filtered out. This is intentional — acting on short bursts would cause excessive taint/untaint cycling (flapping).

5. **Cgroups v1 is not supported.** Some older clusters (RHEL 8, older EKS AMIs) default to cgroups v1. The eBPF hooks and cgroup resolution logic are designed for v2 only. There is no plan to add v1 support — the ecosystem is moving to v2.

6. **The system cannot detect contention that doesn't manifest at the kernel level.** Application-level contention (e.g., two pods competing for the same external database, Redis key conflicts) is invisible to eBPF. node-sentinel detects infrastructure-level contention only.

---

## 19. Versioning and Compatibility `[Core]`

### 19.1 CRD Versioning

CRDs start at `v1alpha1`. Version evolution follows Kubernetes API conventions:

| Version | Meaning | Backward compatible? |
|---------|---------|---------------------|
| `v1alpha1` | Initial design, field names and semantics may change between releases | No — breaking changes allowed |
| `v1beta1` | Stable field semantics, only additive changes | Yes — new fields have defaults |
| `v1` | GA, no breaking changes | Yes — indefinitely |

**Migration strategy:** When promoting from `v1alpha1` → `v1beta1`, ship a conversion webhook that translates old CRs to the new schema. Old CRs continue to work until the operator explicitly migrates them.

### 19.2 gRPC Contract Versioning

The `sentinel.proto` contract uses protobuf field numbering for backward compatibility:

- New fields are added with new field numbers (never reuse deleted numbers)
- Agent and controller may run different versions during rolling upgrades
- The controller must tolerate missing fields (treats as zero/default)
- The agent must tolerate unknown `AgentDirective` types (logs and ignores)

**Version negotiation:** The agent includes its version in the initial gRPC handshake. The controller logs version mismatches and emits a `sentinel_version_mismatch` metric. No hard failure — best-effort compatibility.

### 19.3 Upgrade Path

Rolling upgrades are supported. The recommended order:

1. Upgrade controller first (it's backward-compatible with old agent reports)
2. Upgrade agents via DaemonSet rolling update
3. Apply new CRD schema (if changed)
4. Update NodeHealthPolicy CRs to use new fields (if desired)

At no point during this sequence is the cluster unprotected — agents continue reporting to the old or new controller, and existing taints/cordons persist independently.

---

## 20. Sample Configurations `[Core]`

### 20.1 Conservative Policy (recommended for initial rollout)

Start with observe-only, then graduate to alerting after validating detection accuracy.

```yaml
apiVersion: sentinel.io/v1alpha1
kind: NodeHealthPolicy
metadata:
  name: conservative
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  priority: 100
  mode: alert    # observe → alert → enforce (graduate manually)
  observers:
    cpu:
      enabled: true
      runQueueLatency:
        enable: true
    blockIO:
      enabled: true
      ioLatency:
        enable: true
    network:
      enabled: true
      tcpRetransmits:
        enable: true
    memory:
      enabled: false
    numa:
      enabled: false
  thresholds:
    warning:
      cpuRunQueueLatencyP99: 30ms     # relaxed — avoid false alarms
      ioLatencyP99: 150ms
      tcpRetransmitRate: 5%
    critical:
      cpuRunQueueLatencyP99: 80ms
      ioLatencyP99: 400ms
      tcpRetransmitRate: 12%
    emergency:
      cpuRunQueueLatencyP99: 150ms
      ioLatencyP99: 600ms
      tcpRetransmitRate: 20%
  attribution:
    maxOffenders: 3
    confidenceThreshold: 0.8          # higher bar before alerting
    actionConfidenceOverrides:
      taint: 0.8
      cordon: 0.9
      evict: 0.95
  evaluation:
    window: 3m                        # longer window — more data before deciding
    interval: 10s
    minSamples: 12                    # require more samples
  anomalyDetection:
    burstFilterWindow: 30s            # ignore short spikes
  lifecycle:
    maxActionsPerHour: 3
    deescalationReadings: 5
```

### 20.2 Aggressive Policy (for latency-sensitive environments)

For clusters where API latency SLOs are tight and fast remediation is worth the risk of occasional false positives.

```yaml
apiVersion: sentinel.io/v1alpha1
kind: NodeHealthPolicy
metadata:
  name: aggressive
spec:
  nodeSelector:
    matchLabels:
      sentinel.io/tier: latency-sensitive
  priority: 200                       # overrides conservative policy
  mode: enforce
  observers:
    cpu:
      enabled: true
      runQueueLatency:
        enable: true
      schedDelay:
        enable: true
    blockIO:
      enabled: true
      ioLatency:
        enable: true
      ioThroughput:
        enable: true
    network:
      enabled: true
      tcpRetransmits:
        enable: true
      packetDrops:
        enable: true
  thresholds:
    warning:
      cpuRunQueueLatencyP99: 10ms     # tight thresholds
      ioLatencyP99: 50ms
      tcpRetransmitRate: 2%
    critical:
      cpuRunQueueLatencyP99: 25ms
      ioLatencyP99: 150ms
      tcpRetransmitRate: 5%
    emergency:
      cpuRunQueueLatencyP99: 50ms
      ioLatencyP99: 300ms
      tcpRetransmitRate: 10%
  attribution:
    maxOffenders: 3
    confidenceThreshold: 0.6          # lower bar — act faster
    actionConfidenceOverrides:
      taint: 0.6
      cordon: 0.7
      evict: 0.85
  evaluation:
    window: 1m                        # shorter window — faster detection
    interval: 5s
    minSamples: 6
  anomalyDetection:
    burstFilterWindow: 10s
  evictionPolicy:
    allow:
      ownerKinds: [Deployment, ReplicaSet, Job]
    deny:
      ownerKinds: [StatefulSet, DaemonSet]
      namespaces: [kube-system, sentinel-system]
    respectPDB: true
    fallbackAction: Cordon
  lifecycle:
    cooldownAfterAction: 3m
    maxActionsPerHour: 10
    deescalationReadings: 3
  schedulerFeedback:
    enabled: true
    clearOnRecovery: true
```

---

## 21. Debugging and Troubleshooting Workflows `[Core]`

### 21.1 "Sentinel tainted my node but I don't see contention"

This is the most common false-positive investigation.

```
Step 1: Check the event
  kubectl describe node <node> | grep sentinel
  → Look at sentinel.io/offenders annotation for the blamed pod

Step 2: Check attribution confidence
  kubectl get nodecontentionstatus <node> -o yaml
  → Look at topOffenders[].confidence
  → If confidence < 0.8, attribution may be weak

Step 3: Check live contention on the node
  kubectl exec -it -n sentinel-system <agent-pod> -- sentinelctl top
  → Is contention still visible? If not, it may have been transient

Step 4: Check the offender's actual resource usage
  kubectl top pod <offender-pod> -n <namespace>
  sentinelctl baseline <namespace>/<pod>
  → Compare current vs baseline. Is the pod actually spiking?

Step 5: If false positive confirmed
  → Remove taint: kubectl taint nodes <node> sentinel.io/noisy-neighbor-
  → Consider raising confidence thresholds in the policy
  → File an issue with sentinelctl dump-maps output for analysis
```

### 21.2 "Sentinel isn't detecting obvious contention"

```
Step 1: Verify agent is running and healthy
  kubectl get pods -n sentinel-system -l app=node-sentinel-agent
  kubectl exec -it <agent-pod> -- sentinelctl observers
  → All expected observers should show "attached"

Step 2: Check if contention is visible to eBPF
  sentinelctl top
  → If metrics show elevated latency, the agent sees it
  → If metrics are flat, the contention may be application-level (not kernel)

Step 3: Check if reports reach the controller
  sentinel_reports_received_total metric in Prometheus
  → If zero for this node, gRPC is disconnected

Step 4: Check policy thresholds
  kubectl get nodehealthpolicy -o yaml
  → Are warning thresholds higher than the actual contention?
  → Is the policy in "observe" mode?

Step 5: Check evaluation window
  → Has contention persisted longer than evaluation.window?
  → Are minSamples met?
```

### 21.3 "Sentinel keeps flapping between tainted and untainted"

```
Step 1: Check cooldown and de-escalation settings
  lifecycle.cooldownAfterAction → increase if too short
  lifecycle.deescalationReadings → increase to require more healthy readings

Step 2: Check burst filter
  anomalyDetection.burstFilterWindow → increase to filter longer spikes

Step 3: Look at the contention pattern
  sentinel_evaluation_total metric → plot severity over time
  → If it oscillates, the workload itself is bursty

Step 4: Consider raising thresholds
  → If the contention level is borderline, the thresholds may be too tight
  → Move from aggressive to conservative policy
```

---

## 22. Benchmarking and Validation Plan `[Core]`

### 22.1 Overhead Validation

**Test setup:**
- 3-node cluster (8 CPU, 32GB each)
- Baseline: run standard workload (nginx + postgres + batch jobs) for 1 hour without sentinel
- With sentinel: run identical workload for 1 hour with sentinel agent enabled

**Metrics to compare:**
- Application-level latency (p50, p99) — must not degrade by > 1%
- Node CPU utilization — sentinel agent overhead must be < 1% of total
- Node memory — sentinel agent RSS must be < 50MB
- eBPF program execution time — measured via `bpf_prog_info` stats

### 22.2 Detection Accuracy Validation

**Synthetic contention injection:**
- Use `stress-ng` to generate controlled CPU, I/O, and memory pressure from a specific pod
- Verify sentinel correctly identifies the stress pod as the offender
- Vary intensity to test threshold boundaries
- Run multiple stress pods to test multi-offender detection

**Metrics:**
- True positive rate: correctly identified offender when contention exists
- False positive rate: incorrectly blamed a pod that wasn't causing contention
- Attribution confidence vs. actual cause (ground truth from stress-ng)
- Detection latency: time from contention start to first alert

### 22.3 Safety Validation

**Eviction safety testing:**
- Attempt to evict StatefulSet pods → must be denied
- Attempt to evict DaemonSet pods → must be denied
- Attempt to evict with PDB violation → must be denied
- Verify fallback action (cordon) executes when eviction is denied

### 22.4 Scale Testing

**Large cluster simulation:**
- Use kwok (Kubernetes WithOut Kubelet) to simulate 1000+ nodes
- Deploy sentinel controller and feed synthetic ContentionReports via gRPC
- Measure: evaluation cycle latency, memory usage, gRPC throughput
- Identify the node count at which sharding becomes necessary

---

## 23. Implementation Phases

This is the build order, not the design scope. Everything above is the complete system design. Below is how we ship it incrementally.

### Phase 1: Foundation — Single Observer, Standalone Agent

**Goal:** Get eBPF data flowing from kernel to Go, prove the approach works.

**Scope:**
- `sched_monitor.bpf.c` (CPU scheduling contention only)
- Go agent with map reader, cgroup resolver, Prometheus metrics
- Standalone mode only (no controller, no gRPC)
- `sentinelctl top` CLI for local debugging
- Run as a standalone binary on a test node (not yet DaemonSet)

**Exit criteria:**
- Agent loads BPF program on kernel 5.15+
- Correctly attributes CPU run queue latency to specific pods
- `sentinelctl top` shows live per-pod contention metrics
- Overhead < 1% CPU measured on test node

### Phase 2: Controller + CRD + Alerting

**Goal:** Cluster-aware detection with policy-driven alerting.

**Scope:**
- Add `blkio_monitor.bpf.c` and `net_monitor.bpf.c`
- Controller with gRPC server, policy evaluator, event emitter
- `NodeHealthPolicy` CRD with validation webhook
- `NodeContentionStatus` CR
- Decision engine (collect → validate → aggregate → classify)
- DaemonSet + Deployment packaging
- Policy mode: `observe` and `alert` only (no remediation yet)

**Exit criteria:**
- Agent reports to controller via gRPC
- Controller evaluates policies and emits K8s events
- NodeContentionStatus accurately reflects node state
- `observe` and `alert` modes work correctly
- Synthetic stress-ng test: correct detection within 2 evaluation windows

### Phase 3: Attribution + Automated Remediation

**Goal:** Identify offenders and take action.

**Scope:**
- Multi-offender attribution algorithm (Section 7.5)
- Confidence scoring with per-action thresholds
- Taint/cordon lifecycle (Section 7.7)
- Eviction with full safety model (Section 7.6)
- Cooldown, recovery, de-escalation logic
- Policy mode: `enforce`
- Burst filtering and EMA smoothing

**Exit criteria:**
- Correctly identifies top-N offenders in synthetic multi-pod contention
- Confidence scoring gates actions appropriately
- Eviction denied for StatefulSet/DaemonSet/PDB violations
- Taint lifecycle works end-to-end (taint → cooldown → recovery → untaint)
- All three real-world scenarios (Section 17) reproduce correctly

### Phase 4: Scheduler Feedback + NUMA

**Goal:** Close the loop — prevent recurrence, support bare-metal.

**Scope:**
- Scheduler annotation-based feedback (Section 6.7)
- Scheduler plugin (optional, for advanced users)
- `numa_monitor.bpf.c` (Section 7.10)
- Per-NUMA contention detection and reporting
- NUMA-specific annotations

**Exit criteria:**
- Scheduler avoids placing new pods on contended nodes
- NUMA-localized contention correctly detected on dual-socket test node
- NodeContentionStatus shows per-NUMA state

### Phase 5: Scale + Hardening + Extensibility

**Goal:** Production-ready for large clusters.

**Scope:**
- Controller sharding (Section 10.2)
- `mm_monitor.bpf.c` (memory pressure, ships disabled)
- Observer plugin interface (Section 15)
- Full benchmarking suite (Section 16)
- Helm chart with sensible defaults
- Documentation: operational runbook, troubleshooting guide

**Exit criteria:**
- Controller handles 1000+ nodes without backpressure
- Overhead budget validated (Section 16)
- Helm install works on GKE, EKS, AKS, and bare-metal kubeadm
- Operational runbook covers: install, upgrade, troubleshoot, disable

---

*End of Design Document*
