# Architecture — node-sentinel at a glance

How the pieces fit, in three pictures: **where it runs**, **how data flows**, and **how one agent turns kernel events into a verdict**. Plain-English model is in [`CONCEPTS.md`](CONCEPTS.md); the eBPF build/run mechanics are in [`HOW.md`](HOW.md); the formal design is in [`docs/`](.).

Each box is tagged ✅ **built** or 🔜 **roadmap** so you can tell today's behaviour from where it's heading.

---

## 1. Where it runs — deployment topology

One **agent** per node (a DaemonSet), one **controller** per cluster (a Deployment). The agent is self-contained: it works even with no controller. The controller only *aggregates* today — it does not act.

```mermaid
flowchart TB
    operator(["Operator"])

    subgraph cluster["Kubernetes Cluster"]
        direction TB

        subgraph node1["Worker Node"]
            direction TB
            pods1["Application pods<br/>(share CPU / disk / NIC)"]
            kernel1{{"Linux kernel<br/>eBPF programs attached"}}
            agent1["node-sentinel agent<br/>(DaemonSet pod)"]
            kernel1 -. "observes contention" .-> pods1
            agent1 <--> |"reads kernel maps"| kernel1
        end

        subgraph node2["Worker Node"]
            direction TB
            pods2["Application pods"]
            kernel2{{"Linux kernel<br/>eBPF programs"}}
            agent2["node-sentinel agent"]
            kernel2 -. "observes" .-> pods2
            agent2 <--> kernel2
        end

        controller["node-sentinel controller<br/>(Deployment · 1 replica)<br/>aggregates per-node reports ✅<br/>acts on offenders 🔜"]

        agent1 -->|"per-node report (HTTP)"| controller
        agent2 -->|"per-node report (HTTP)"| controller
    end

    operator -->|"sentinelctl top / status<br/>(kubectl exec)"| agent1
    operator -->|"GET /status · /metrics"| controller
    controller -. "taint / cordon / evict 🔜" .-> cluster
```

**Read it as:** every node watches its own kernel and reports up; the operator can look at a single node live (`sentinelctl`) or the whole cluster (the controller). The dashed arrow back into the cluster — remediation — is the roadmap half.

---

## 2. How data flows — the forward-only pipeline

Each stage hands off to the next and **never calls back**. Signals are born in the kernel and travel one direction: kernel → agent → controller → Kubernetes.

```mermaid
flowchart LR
    K["eBPF · kernel<br/>collect signals<br/><br/>per-cgroup log2<br/>histograms in<br/>per-CPU maps<br/><br/>✅ built"]
    A["Agent · Go<br/>aggregate + detect<br/><br/>merge per-CPU,<br/>estimate percentiles,<br/>resolve cgroup → pod,<br/>judge vs. baseline<br/><br/>✅ built"]
    C["Controller · Go<br/>decide + act<br/><br/>aggregate reports ✅<br/>confidence gate 🔜<br/>pick remediation 🔜"]
    E["Kubernetes<br/>enforce<br/><br/>taint / cordon /<br/>evict offender<br/><br/>🔜 roadmap"]

    K -->|"read-and-delete<br/>snapshot / interval"| A
    A -->|"per-node report"| C
    C -->|"action 🔜"| E

    style K fill:#e8f4ea,stroke:#3a7d44
    style A fill:#e8f4ea,stroke:#3a7d44
    style C fill:#fff6e5,stroke:#b8860b
    style E fill:#fdeaea,stroke:#b03a3a
```

Green = built and running today. Amber = partially built (the controller aggregates but doesn't yet decide). Red = roadmap.

---

## 3. Inside one agent — kernel events to a verdict

What happens between "a task wakes up" and "this pod is OVER its fair share." Three observers feed the same detection path, once per interval (default 5s).

```mermaid
flowchart TB
    subgraph kernel["Kernel space — eBPF (per node)"]
        direction LR
        o1["sched observer<br/>run-queue latency<br/>(CPU victim signal)<br/>+ CPU time (offender)"]
        o2["blkio observer<br/>block-I/O latency<br/>+ throughput"]
        o3["net observer<br/>NIC queueing<br/>+ retransmits"]
        maps[("per-cgroup log2 histograms<br/>in per-CPU hash maps")]
        o1 --> maps
        o2 --> maps
        o3 --> maps
    end

    subgraph agent["User space — Go agent"]
        direction TB
        snap["Snapshot<br/>read-and-delete each interval,<br/>sum the per-CPU copies"]
        pct["Percentiles<br/>log2 histogram → p50 / p99<br/>(bucket midpoint estimate)"]
        resolve["Resolve<br/>cgroup_id → namespace/pod/container<br/>via the CRI socket"]
        judge["Judge (per dimension)<br/>• vs. learned baseline (xBASELINE)<br/>• unusual and actually bad?<br/>• how much? (intensity vs. request)<br/>• confidence score"]
        report["Report<br/>offenders + victims per dimension<br/>→ stdout · /metrics · sentinelctl · controller"]
        snap --> pct --> resolve --> judge --> report
    end

    maps -->|"every interval"| snap

    note["Safety rule: a cgroup with no CRI container<br/>(system slices, pause sandboxes) resolves to<br/>unknown and is never blamed."]
    resolve -.-> note
```

**The honest-attribution guarantee lives here:** if a cgroup can't be tied to a real Kubernetes container, it's labelled `unknown` and never attributed — so the system would rather say "can't tell" than blame the wrong pod. That same caution is why offenders carry a **confidence** score and low-confidence findings are *alert-only*, never acted on.

---

## Where each box lives in the tree

| Picture box | Package / file |
|---|---|
| eBPF observers (CPU / disk / net) | `internal/ebpf/bpf/*.bpf.c` + `internal/ebpf/{loader,sched,…}.go` |
| Snapshot + percentiles | `internal/ebpf` (read-and-delete) + `internal/metrics/histogram.go` |
| cgroup → pod resolver | `internal/cgroup/resolver.go` |
| Judge / baseline / confidence | `internal/metrics` (judgement) |
| Agent lifecycle | `internal/agent/*` + `cmd/agent/main.go` |
| Controller (aggregate) | `internal/controller/*` + `cmd/controller/main.go` |
| Live CLI | `cmd/sentinelctl` |

Layout follows design §7.2.1. For the numbers behind each stage (event rates, map sizes, overhead), see [`docs/node-sentinel-internals.md`](node-sentinel-internals.md).
