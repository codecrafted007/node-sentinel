# Detection demo — catching a noisy neighbour, step by step

A reproducible walkthrough of node-sentinel detecting a CPU noisy-neighbour on a **live multi-node Kubernetes cluster**, with the actual output it produced. It shows the part that makes the system trustworthy: it **refuses to blame a pod until it's confident**, and it tells a *spike* apart from a pod that's *simply always busy*.

> Node and third-party workload names below are anonymized (`worker-1`, `app-*`); the agent's own output format and numbers are verbatim from a real run. The model: [`CONCEPTS.md`](CONCEPTS.md) · the confidence code: [`internal/agent/agent.go`](internal/agent/agent.go) (`offenderConfidence`).

---

## The setup

- A 5-node cluster, one **agent** per node (DaemonSet) + one **controller** (Deployment), deployed per [`DEPLOY.md`](DEPLOY.md).
- Target node `worker-1`: **3.5 cores allocatable, ~87% already requested** — little headroom, so a hog there genuinely starves its neighbours.
- The probe we'll trip: CPU contention. The agent reads two signals from one tracepoint — **CPU intensity** (offender: share of CPU consumed) and **run-queue latency** (victim: how long others waited for a core).

---

## Baseline — a busy cluster that still blames no one

Even with no synthetic load, a real cluster has low-grade contention. The agent sees it and stays honest — it flags the contention but **refuses to name an offender** because nothing crosses the **70% confidence gate**:

```
15:23:01  [!] CONTENTION — CPU: 1, I/O: 0, NET: 0 victim(s)
  ── CPU ──  attribution: low confidence (68% < 70% threshold) — alert only
  OFFENDERS — by CPU time
  POD                                    CPU_MS INTENSITY   REQ_m CONFIDENCE  VERDICT
  kube-system/cilium-agent                  193     12.1%     194         5%  OVER fair share (5.9%)
  app-1/log-shipper                         112      7.0%       1         1%  OVER fair share (0.0%)
  app-2/config-api                           61      3.8%       1         0%  OVER fair share (0.0%)
```

The highest confidence seen organically across the whole cluster over 15 minutes was **68%** — close, but the agent held the line and never attributed. **That restraint is the feature.**

---

## Attempt 1 — a steady hog (and why it *doesn't* trip the gate)

Drop in a pod that pins **8 busy-loops** on a 3.5-core node — instantly, and stays flat-out:

```yaml
# command: timeout 420 sh -c "for i in $(seq 1 8); do while true; do :; done & done; wait"
# requests.cpu: 100m   (so it's ~8x over its fair share)
```

After ~45s the agent reports:

```
15:39:53  [!] CONTENTION — CPU: 13, I/O: 0, NET: 0 victim(s)
  ── CPU ──  attribution: low confidence (4% < 70% threshold) — alert only
  OFFENDERS — by CPU time
  POD                                    CPU_MS INTENSITY   REQ_m CONFIDENCE  VERDICT
  sentinel-system/noisy-neighbor/hog      17627     88.2%      99         4%  OVER fair share (3.1%)
  system(cg:6700)                           736      3.7%       -          —  system / unattributed
  kube-system/cilium-agent                  247      1.2%     194         0%  within request (6.1%)
```

The hog is plainly the dominant CPU user — **88.2% intensity, way over its fair share, 13 victims** — yet its confidence is **4%**. Why?

Confidence is the **minimum of three signals — all must hold** (`offenderConfidence`):

| Signal | Meaning | This hog |
|---|---|---|
| **magnitude** | holds a meaningful share of the resource | ✅ ~1.0 (88% ≫ the 25% "full" mark) |
| **victimSignal** | neighbours are genuinely degraded | ✅ 13 real victims |
| **changed** | usage is far above the pod's *own learned normal* | ❌ ~0 |

The hog ran flat-out **from birth**, so within ~3 intervals the agent **learned that 88% *is* this pod's normal** → it isn't "changed" → `min(...)` collapses to ~0. By design, node-sentinel won't cry wolf over a pod that is *simply always busy* — only over one that **departs from its own history**. A steady hog can never cross the gate.

---

## Attempt 2 — an anomalous spike (100% confidence)

Now the realistic shape of an incident: a pod that behaves **normally first, then spikes**. Idle 60s to warm a low baseline, *then* burst:

```yaml
# command: timeout 420 sh -c "sleep 60; for i in $(seq 1 8); do while true; do :; done & done; sleep 200"
# requests.cpu: 100m
```

Seconds after the burst begins, the agent catches it:

```
15:43:48  [!] CONTENTION — CPU: 10, I/O: 0, NET: 0 victim(s)
  ── CPU ──  attribution: confident pod offender (100% >= 70% threshold)
  OFFENDERS — by CPU time
  POD                                    CPU_MS INTENSITY   REQ_m CONFIDENCE  VERDICT
  sentinel-system/noisy-spike/hog         18424     92.1%      99       100%  OVER fair share (3.1%)
```

All three signals now hold at once:

- **changed** → 1.0: the pod jumped from a near-idle baseline to 92% — a clear departure from its own normal (and the baseline is *frozen* during the spike, so it stays "changed").
- **magnitude** → 1.0: 92% of the node's CPU.
- **victimSignal** → high: up to **20** neighbouring pods piling up on the run queue.

`min(1.0, 1.0, ~1.0)` → **100%**, and the attribution line flips from *"alert only"* to **`confident pod offender`**.

### The controller sees it cluster-wide

The per-node verdicts roll up into one cluster view — the node with the confident offender stands out from the merely-noisy ones:

```
worker-1  CONTENDED  CPU:20 I/O:0 NET:0  — confident offender (100%)
worker-2  CONTENDED  CPU:2  I/O:0 NET:0  — alert only (no confident offender)
worker-3  CONTENDED  CPU:1  I/O:0 NET:0  — alert only (no confident offender)
```

That `confident offender (100%)` line is exactly the signal the **remediation half** (taint / cordon / evict — 🔜 roadmap) is designed to act on. Detection → attribution → confidence gate → cluster aggregation, all validated end-to-end on real hardware.

---

## What this proves

1. **Honest attribution.** A genuinely busy cluster produced *no* false accusation — the closest organic case (68%) was held below the gate.
2. **Spike vs. steady.** node-sentinel distinguishes a pod *departing from its own normal* (an incident) from a pod that is *constitutionally heavy* (probably fine) — the thing a fixed threshold can't do.
3. **Two-sided measurement.** The offender is found by **CPU intensity**; the harm is proven by **run-queue latency on the victims**. Both are required before anyone is blamed.
4. **The gate is real.** Only a finding ≥ 70% confidence is ever called an offender; everything else is `alert only`.

---

## Reproduce it yourself

```sh
# 1. pick a node with little CPU headroom and pin a spike pod to it
cat <<'YAML' | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: noisy-spike
  namespace: sentinel-system
spec:
  nodeName: <target-node>        # a node with little spare CPU
  restartPolicy: Never
  containers:
    - name: hog
      image: busybox:1.36
      # idle 60s (warm a LOW baseline) -> burst 8 cores (anomalous spike)
      command: ["timeout","420","sh","-c","sleep 60; for i in $(seq 1 8); do while true; do :; done & done; sleep 200"]
      resources:
        requests: { cpu: 100m, memory: 16Mi }
YAML

# 2. watch the agent on that node flip to a confident offender (~70s in)
kubectl -n sentinel-system logs ds/node-sentinel-agent --since=90s | grep -E "attribution|noisy-spike"

# 3. see it in the cluster view
kubectl -n sentinel-system logs deploy/node-sentinel-controller --since=30s | grep confident

# 4. clean up
kubectl -n sentinel-system delete pod noisy-spike
```

> Tip: a hog that's busy *from birth* (no idle phase) will **not** cross the gate — that's the point of step 1's `sleep 60`. To demo the gate holding, drop the `sleep` and watch confidence stay low despite the same 90% load.

For the cluster-deployment recipe (incl. importing the image onto a node without a registry), see [`HELPERCOMMANDS.md`](HELPERCOMMANDS.md); for the plain-English model, [`CONCEPTS.md`](CONCEPTS.md).
