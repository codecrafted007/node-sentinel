# Progress Log

A running record of completed work, newest phase first. Roadmap lives in design §23.

---

## Phase 3 — Remediation (in progress)

Goal: close the control loop — the controller stops being observe-only and *acts* on confident offenders, conservatively and visibly (milestone "Temporal correlation (v0.2)", issue #7).

### Durable /resize throttle state — restart-safe remediation ✅ (production hardening)
- **The gap:** the `/resize` throttle + scheduled restore were in-memory only — a controller restart mid-window orphaned a throttled pod (capped forever). Flagged as a known limitation on the resize PR.
- **The fix** (`internal/controller/persist.go`): active throttles are written to a **ConfigMap (`node-sentinel-throttles`) in the controller's own namespace**, and reconciled on startup (`Recover`) so a restarted controller resumes and restores them. A ConfigMap ledger (not pod annotations) keeps the pod permissions narrow — the controller still only patches `pods/resize`, never the pod spec — and confines its own state to its own namespace.
- **Retry-safe restore:** `restoreDue` only drops an entry (memory + ledger) once the restore succeeds or the pod is gone (`IsNotFound`); a transient failure is retried next tick.
- Single-replica controller → ConfigMap read-modify-write is serialised by a mutex (no optimistic-retry needed). `--policy`/flag startup calls `EnablePersistence(POD_NAMESPACE)` + `Recover` before the restore loop; persistence off (`""`) keeps the prior in-memory behaviour.
- **RBAC**: a namespaced `Role` for `configmaps` (get/create/update) in `sentinel-system` — no cluster-wide ConfigMap access. `POD_NAMESPACE` via the downward API.
- **Tests**: a full **throttle → (fresh controller) recover → restore → forget** round-trip across two Remediator instances over one fake client, plus persistence-disabled-by-default.

### NodeHealthPolicy CRD — declarative remediation policy ✅ (host-verified)
- **CRD** (`deploy/crd.yaml`, `sentinel.io/v1alpha1`, cluster-scoped): the controller's remediation is now declarative instead of flag-only. The core abstraction from the design — **`mode: observe | alert | enforce`** — maps cleanly: observe→aggregate only, alert→Event tier, enforce→`/resize` tier. Plus `attribution.confidenceThreshold` (controller-side gate that can tighten the snapshot's) and a `remediation` block (`resize`, `cooldown`, `restoreAfter`, `namespaces`). Schema `preserveUnknownFields` under spec so the design's richer fields (observers/thresholds) are forward-compatible.
- **`internal/controller/policy.go`**: reads the policy via the **dynamic client** (no generated clientset — keeps the plain-client-go style), picks the highest-`priority` policy, and maps it to a `RemediationConfig` + active flag. Parse + map are pure functions. `cmd/controller --policy` drives remediation from the CRD (overriding the flags); flags remain the fallback path.
- **Faithful to the design where it matches** (`mode`, `attribution`, `nodeSelector`/`priority` naming) but configures what node-sentinel *actually does* (resize/Event tiers), not the design's aspirational taint/cordon/evict.
- **RBAC**: + `nodehealthpolicies` get/list/watch. Sample at `deploy/nodehealthpolicy-sample.yaml`.
- **9 unit tests** (parse full spec, observe-default, mode→config for all four cases, highest-priority selection + none-found via the dynamic fake client).
- 🔜 Live re-watch (today a policy change needs a controller restart), per-node `nodeSelector` matching, and policy-driven *detection* thresholds (still agent flags).

### In-place /resize primary tier — issue #7 (resize) ✅ (host-verified)
- **The primary actuator** (`internal/controller/resize.go`): for a confident CPU offender, patch the pod's **`/resize` subresource** (KEP-1287) to lower its CPU limit to its request — the *kubelet* actuates the cgroup change, nothing to fight. "Timeout, not eviction": the throttle is recorded and **auto-restored** after `--restore-after` (scheduled-restore loop, `RunRestore`). Every throttle and restore is announced by an Event.
- **Tiered**: `act()` tries `/resize` first (CPU only, with `--resize`); falls back to the Event tier when the pod has no CPU limit above its request, or the resize is rejected. Pods without a usable limit, and all disk/net offenders, stay Event-only.
- **Scoped rollout** (`--remediate-namespaces`): a namespace allowlist so actuation can be enabled one namespace at a time — the safe way to adopt it (and how the live test was confined to `sentinel-system`).
- **RBAC**: added `pods/resize: patch`; still no evict/delete and no patch of the pod spec proper.
- **9 unit tests** (fake clientset + injectable clock): throttle lowers the limit to the request, fallback when no room, restore after the window (and not before), resize-disabled is Event-only, namespace allowlist blocks out-of-scope pods.
- **Host-verified on GKE 1.33**: (1) the apiserver exposes `pods/resize`; a strategic-merge patch to it actuates a live limit change (2→100m, HTTP 200; merge-patch correctly 422). (2) The controller detected a 100%-confidence CPU offender in `sentinel-system` and **issued the throttle** (`3→100m`, scoped, logged with restore time). Decision → tier-dispatch → scoping → patch all confirmed live.
- ⚠️ **Known limitation**: the throttle/restore state is **in-memory** — if the controller restarts mid-window, a pending restore is lost and the pod stays throttled until its next restart/redeploy. A durable store (or reconciling from the throttle Events / a label) is a follow-up before production use.

### Network confidence: judge the victim on retransmit *rate*, not raw count — issue #12 ✅ (host-verified)
- **Root cause:** `netVictims` flagged a cgroup on raw retransmit *count* (`≥ RetransWarn`), so any high-throughput pod (metrics agents, ingress) registered as a "victim" just by sending a lot — inflating `victimSignal`, which (with concentrated TX `magnitude`) carried offenders to a spurious 100%. Surfaced live during #7: dozens of innocent pods got `NoisyNeighborThrottled` Events.
- **Fix** (`agent.go` `netVictims` + `config.go`): a network victim now must clear three gates — `MinSegs` (enough activity), `RetransWarn` (a meaningful absolute count), **and** `RetransRateWarn` (retransmit *rate* = retransmits/segs, default 1%) — and the baseline now learns each pod's normal *rate*, so we flag a rate that's both genuinely high and unusual *for itself*. A pod at 10 retransmits / 10 000 segs (0.1%) is no longer a victim; one at 20% is.
- **Live before/after** on the GKE cluster: before, confident net offenders were a broad sweep of innocent busy pods (vmagent, kube-state-metrics, argocd, redis, …); after, they narrowed to pods with genuinely pathological retransmit rates (`qablrupgrd/ingresscfg` at 27–47%), with the innocent high-volume pods dropping to low/no confidence and the gate discriminating within survivors (50% → alert-only, cold-baseline → `—`).
- New flag `--retrans-rate-warn`; `--retrans-warn` is now the secondary absolute-count gate.

### Tiered remediation — Event tier + framework (`internal/controller/remediation.go`) — issue #7 ✅ (host-verified)
- The controller now has a Kubernetes client (`client-go`, in-cluster) and a `Remediator`. **Off by default** — observe-only unless `--remediate`, with `--dry-run` and a per-pod `--cooldown` (default 5m).
- **Decision engine:** acts only on offenders the per-node confidence model already marked confident (`Confidence >= ConfidenceMin`), across CPU/disk/net; `system(cg:..)`/`unknown` are never acted on (the honest-attribution rule, unit-tested in `splitPod`).
- **Mandatory Event tier:** emits a `Warning` Event (`Reason: NoisyNeighborThrottled`) anchored to the offending pod (looked up for UID), so a throttled pod is never a silent mystery. Per-pod cooldown prevents Event spam.
- **RBAC** (`deploy/rbac.yaml`): a minimal ClusterRole — `events: create/patch`, `pods: get/list`. No evict/delete/patch; the mandatory tier only emits Events.
- **8 portable unit tests** (fake clientset + injectable clock): gate, dimension extraction, Event anchoring/Reason/Type, cooldown window + elapse, dry-run emits nothing, low-confidence skipped.
- **Host-verified** on the GKE cluster: with `--remediate` live, a 100%-confidence offender (`noisy-neighbor-remediate`) got a `NoisyNeighborThrottled` Warning Event on the pod; cooldown held; low-confidence stayed alert-only. Then disabled + cleaned up.
- 🔎 **Finding (follow-up):** with remediation live, the **network dimension flagged many real pods as 100%-confident offenders** — the net confidence (retransmit victim + share-of-bytes magnitude) appears mis-calibrated and over-fires. Separate from #7; worth a calibration issue before enabling remediation broadly.
- 🔜 Next tier: in-place `/resize` (KEP-1287) as the primary actuator, with fallback to this Event tier.

## Phase 2 — Temporal correlation (in progress)

Goal: attribute a victim's stalls to the offender that caused them by the *shape* of bursts over sub-interval time, not magnitude (milestone "Temporal correlation (v0.2)", issues #2–#7).

### TTL'd resolver cache so late stats stay nameable — issue #3 ✅ (host-verified)
- `internal/cgroup/ttlcache.go`: the `cgroup_id -> PodID` cache now keeps a vanished cgroup's name for a grace period (`CacheTTL`, default 30s ≈ 6 read intervals) instead of dropping it the instant the cgroup disappears — so a histogram captured in a pod's *final* interval still resolves instead of going `unknown`. Live entries never expire; an entry only ages out once absent (or tombstoned) past the TTL. Injectable clock → **6 portable unit tests on macOS** (live-never-expire, grace survival, deadline-doesn't-reset, get-rejects-expired, tombstone, put).
- `PodID` moved to a portable file so the cache + tests build anywhere; resolver `Refresh` now *merges* (`cache.replace`) rather than wholesale-replacing.

### In-kernel cgroup lifecycle watcher — issue #2 ✅ (host-verified)
- `internal/ebpf/bpf/cgroup_monitor.bpf.c`: `tp_btf/cgroup_mkdir` + `cgroup_rmdir` emit `(cgroup_id, op, path)` to a **BPF ring buffer** (first ringbuf in the project); the kernel side just records + emits, all parsing/CRI join is in Go. A short-lived container is named at *birth* (lazy `ContainerStatus` join → `cache.put`) and tombstoned at *death*, far more reliably than fsnotify+rescan (which drops events under load / sub-debounce churn).
- `internal/ebpf/cgroup.go`: `CgroupObserver` loads + attaches the tracepoints and streams events via `ringbuf.Reader`; the agent runs a goroutine dispatching mkdir→`ResolveCgroupPath`, rmdir→`Tombstone`. Best-effort (fsnotify is the fallback).
- **Host-verified** on all 5 GKE nodes: the two tp_btf programs pass the verifier, the ringbuf attaches, and a short-lived (~8s) burster was attributed **by name** (`sentinel-system/shortlived-hog/hog`, not `system(cg:…)`) with zero lifecycle/ringbuf errors under churn. (Required a `_unused_cgroup_event` global to force BTF emission for bpf2go's `-type`, since a ringbuf has no value type.)
- Known limit: if CRI hasn't registered the container at `cgroup_mkdir` time the lazy join no-ops and the periodic rescan is the backstop; tombstone + TTL (#3) cover capture-at-death regardless.

### Sub-interval time-bucketed histograms (`sched_monitor.bpf.c` + reader) — issue #4 ✅ (host-verified, all 5 GKE nodes)
- Extended the sched observer (no new probes): `sched_switch` now also writes a per-cgroup **ring of 64 × 100ms buckets** (`sched_timeline_map`, `PERCPU_HASH`) — `runq_lat_ns`/`runq_count` (victim) charged to `next`'s bucket, `cpu_ns`/`ctx_switches` (offender) to `prev`'s. Lock-free integers only; all scoring stays in Go.
- **Epoch-per-bucket lazy reset:** each slot stores the absolute window number it holds; when the ring wraps onto a stale slot the epoch mismatches and it's zeroed before use, so an unaligned drain never mixes two windows (per the design note).
- **Memory bounded:** `max_entries = MAX_ACTIVE_CGROUPS (512)`, not `MAX_CGROUPS`, so cost (per-CPU × 64 × 32B × entries) stays a few MB instead of ballooning (~3 MB at 4 vCPU).
- **`ReadTimeline()`** (`internal/ebpf/sched.go`): read-and-delete drain that re-aligns the per-CPU copies by epoch onto **one shared axis ending at `now`** (CLOCK_MONOTONIC, matching `bpf_ktime`) — essential because offender and victim are different cgroups and must share a time axis to correlate. Returns `[]CgroupTimeline` (`types.go`), zero-filling empty windows — exactly the aligned `[]float64`-shaped series `metrics.Correlate` consumes.
- Agent drains the ring each interval (keeps the bounded map clean) and stashes the latest for the scorer; bpf2go `-type` extended for the new structs; `x/sys` promoted to a direct dep for the monotonic clock.
- **Two host-only bugs the GKE build caught** (macOS can't compile/load eBPF): (1) a 1600-byte `struct sched_buckets` zeroed on the BPF stack blew the 512-byte stack limit → fixed with a zeroed per-CPU `timeline_zero` scratch map as the insert template; (2) a `% NUM_BUCKETS` ring index the verifier couldn't bound (`math between map_value pointer and register with unbounded min value`) → made `NUM_BUCKETS` a power of two (64) and index with a bitmask. Verified live: agent loads + attaches on all 5 nodes (Ubuntu 24.04, kernel 6.8), `ReadTimeline` drains every interval with zero errors.

### cpu.stat nr_throttled as a confidence input (`cgroup/cpustat.go` + agent) — issue #6 ✅ (host-verified)
- **Portable parser** `internal/cgroup/cpustat.go`: reads cgroup v2 `cpu.stat` (`nr_periods`/`nr_throttled`/`throttled_usec`), with `Sub` (monotonic per-interval delta, clamps counter resets) and `ThrottledFraction` (share of CFS periods throttled). Pure text handling → unit-tested on macOS (4 tests, ✅).
- **Resolver** now records each cgroup's dir during the scan and exposes `ReadCPUStat(cgroupID)` (plain cgroupfs read, no probe).
- **Agent** tracks last interval's `cpu.stat` per offender cgroup, computes the throttle delta, surfaces it as a `THROTTLE` column (stdout) + `sentinel_pod_cpu_throttle_ratio` (Prometheus), and folds it into offender confidence: throttle **stands in for the CHANGED signal** via `max(changed, throttle)`, so it corroborates a spike and works before the learned baseline is warm; magnitude + harm still gate via `min`.
- **Live-validated** on the GKE node: an *unlimited* hog (53.8% CPU, no throttle) scored **0%** confidence — efficiently busy, its load is its own normal — while a *quota-capped* hog (25.2% CPU, throttled 100% of periods) scored **100%**. Throttle correctly promoted the disruptively-bursty pod over the higher-CPU steady one, exactly issue #6's intent. No errors.

### Lagged correlation scorer (`internal/metrics/correlation.go`) — issue #5 ✅
- Pure-Go, unit-tested on any platform (mirrors `histogram.go`), ported from `docs/sim/temporal-correlation.html` (sim #2). The kernel only accumulates integers; all floating-point scoring stays here, hot-swappable.
- `Correlate(offender, victim, cfg)` returns the strongest **positive lagged Pearson** correlation over the (future) sub-interval bucket series. Pearson is scale/offset invariant, so it scores co-movement, not height — "busy ≠ guilty".
- **Anti-false-positive guards** (issue #5): lag search with offender-precedes-victim causality guard; min-active-bucket gate on *both* series; variance floor (blocks flat-but-loud series); `Confidence()` clamps the anti-correlated half to 0. A gated result is "not attributable", not "innocent".
- `correlation_test.go`: 11 tests (perfect/scaled correlation, lag detection, busy≠guilty, anti-correlation, activity gate, variance gate, independent-spike rejection, length mismatch, flat-series, variance). **Portable — runs on macOS.** ✅ passing.

### Wire the scorer into the agent — issue #5 integration ✅ (host-verified)
- Each interval the agent pairs every CPU offender's on-CPU bucket series (`CpuNs`) against the CPU victims' run-queue series (`RunqLatNs`) from the drained #4 timeline (shared epoch axis), takes the best lagged correlation, and surfaces it as a `CORREL` column (stdout) + `sentinel_pod_cpu_burst_correlation` (Prometheus).
- **Folded conservatively:** offender-specific harm = `max(node-wide victimSignal, correlation)`, so a strong shape match raises this pod's harm linkage — but `changed` + `magnitude` still gate via `min`, and correlation is gated to 0 on sparse timelines, so it can only corroborate, never invent an offender ("the anomaly-vs-baseline gate stays on top", per the issue).
- **Live-verified** on GKE: correlation computes real values from the timeline (14–26% for a near-sustained hog) and nudges confidence (CORREL 25% → confidence ticked up), folding in with zero errors. Correctly stays **low for a steadily-busy pod** (shape-not-magnitude working). NB: a clean "correlation-as-decider" demo needs a genuinely intermittent multi-core workload — busybox background loops survive `timeout`, so a crisp duty cycle was hard to choreograph; the threshold tuning (`ActiveFloor`/`MinActive`) and that demo are follow-ups, not code gaps.

---

## Phase 1 — Foundation (in progress)

Goal: prove kernel→Go run-queue-latency attribution with a standalone agent (design §23 Phase 1).

### Build toolchain & project scaffolding
- Created the Go module and package layout per design §7.2.1 (`cmd/agent`, `internal/{agent,ebpf,cgroup,metrics}`).
- `Makefile` with `setup / vmlinux / generate / build / agent / test / clean`; bpf2go (`cilium/ebpf`) for compiling BPF C and generating Go bindings via CO-RE.
- `.gitignore` treats generated artifacts (`sched_bpfel.go`, `*.o`, `vmlinux.h`) as build output — repo is source-only.

### Histogram → percentile math (`internal/metrics`)
- `histogram.go`: estimate percentiles from in-kernel log2 histograms (bucket midpoint `2^i * 1.5`); `Mean` from summed total/count.
- `histogram_test.go`: 5 unit tests (empty, single-bucket, split distribution, mean, midpoint). **Portable — runs on macOS.** ✅ passing.

### eBPF scheduler observer (`internal/ebpf/bpf/sched_monitor.bpf.c`)
- CO-RE BPF using `tp_btf` raw tracepoints: `sched_wakeup`/`sched_wakeup_new` stamp wakeup time per pid; `sched_switch` computes `now − wakeup` for the incoming task = run-queue latency.
- Per-cgroup log2 histogram (µs) in a `PERCPU_HASH` (lock-free at high `sched_switch` rates); transient wakeup timestamps in a plain `HASH`.

### BPF loader & map reader (`internal/ebpf/{loader,sched,types}.go`)
- `loader.go`: remove memlock rlimit, load embedded objects, attach the three tracepoints via `link.AttachTracing`.
- `sched.go`: read-and-delete snapshot per interval — sum per-CPU histogram copies, then delete the key (design §7.2.3).

### Agent lifecycle (`internal/agent`, `cmd/agent`)
- `config.go` (portable) with Phase-1 defaults; `agent.go` (linux) load→read-loop→print worst cgroups by p99; thin `main.go` for flags + signals.

### Verified on real hardware (Linux build host)
- Installed toolchain on the host (Go 1.25.6, clang 14, llvm, libbpf-dev, bpftool); worked around a broken apt state (`--fix-broken`).
- BPF C compiled clean, **verifier accepted it at load**, tracepoints attached, agent printed live per-cgroup run-queue latency.
- **Stress test:** `stress-ng` at 4× CPU oversubscription drove node-wide run-queue **p99 from <3 ms to 6–12 ms** — the noisy-neighbor signature, straight from the kernel.
- Insight recorded: run-queue latency is a **victim-side** signal; offender attribution needs the separate CPU-time intensity signal (§7.5 step 2, not yet built).

### cgroup → pod resolver (`internal/cgroup/resolver.go`)
- Maps `cgroup_id` → `namespace/pod/container` (design §7.4): the cgroup_id eBPF reads equals the cgroup directory **inode** (exact on kernels ≥ 5.5), so it scans the cgroup tree for container scopes and joins against containerd's CRI `ListContainers` labels.
- Cgroups with no CRI container (system slices, pause sandboxes) resolve to `unknown` and are never attributed.
- Wired into the agent (initial scan + periodic refresh) with `--cri-socket` / `--cgroup-root` flags; resolution is best-effort (agent still runs if CRI is down).

### Verified against live Kubernetes
- Ran on the host's single-node cluster (K8s v1.35.0, containerd, systemd cgroup driver): **73 containers mapped**, agent printed real pod names (`default/nascontroller-…`, `kube-system/kube-proxy-…`) instead of raw inode numbers.

### Repository setup & documentation
- Module path set to `github.com/codecrafted007/node-sentinel`; git remote `origin` → `git@github.com:codecrafted007/node-sentinel.git`.
- Added `CLAUDE.md` (guidance for future Claude Code sessions), a dev-friendly `README.md` (build host requirements, quick start, remote-dev loop, troubleshooting), and this `PROGRESS.md`.
- Added `build.sh` — one-shot build (preflight checks → BTF dump → bpf2go → build every `cmd/`), with `--setup/--tidy/--skip-generate` and a non-Linux guard.
- Added `HOW.md` — junior-dev onboarding explainer: how the eBPF probe is compiled, embedded (bpf2go + `go:embed`), loaded (`LoadAndAssign`/verifier), and attached, with a tour of the generated `sched_bpfel.go`.
- Added `CONCEPTS.md` — plain-English explanation of what the system does and how it decides (shared-apartment / fitness-watch / dinner-platter / smoke-alarm analogies), with each idea tagged ✅ built vs 🔜 planned.
- Added `stress-test.sh` — an **acceptance test** for the detector: asserts baseline=`healthy` → under `stress-ng`=`CPU CONTENTION` → recovery=`healthy`, prints PASS/FAIL per phase, exits non-zero on failure (CI-gateable). Options `--workers/--duration/--interval/--top`. Verified green end-to-end.

### Live-validated on the cluster
- Ran the full baseline→inject→recover flow against the running single-node cluster: run-queue p50 jumped from ~2–3 µs to 12–768 µs (p99 into 6–24 ms) across real 5G-core pods under 48 CPU hogs, then recovered within seconds. Pod names kept resolving via CRI even while the cluster's `kubectl`/API discovery was degraded — confirming the resolver's independence from the kube API (§7.4).
- Made dependencies deterministic: committed `go.sum`, added `tools.go` to track the `bpf2go` tool, and pinned deps for Go 1.25 (`k8s.io/cri-api` v0.32.3 — v0.33+ needs Go 1.26); `build.sh` forces `GOTOOLCHAIN=local`.

---

### Offender signal — per-cgroup CPU intensity (§7.5 step 2)
- Extended the `sched_switch` hook to also charge each on-CPU slice to the outgoing task's cgroup (`cpu_time_map`, with a per-CPU `cpu_slice_start`); idle/first-sample skipped.
- Agent now prints two views every interval: **OFFENDERS** (CPU intensity = a cgroup's share of CPU time consumed) and **VICTIMS** (run-queue latency).
- Live-validated: under a CPU hog, its cgroup tops the offenders table at ~91% intensity while real pods' run-queue latency climbs — the offender the victim-side signal alone could not surface.
- Rewrote `sched_monitor.bpf.c` in plain K&R style (readable `log2_bucket` loop, full-word names) per user preference; reloaded verifier-clean.

### Judgement layer — quiet unless genuinely contended
- Problem: run-queue latency and CPU use are never zero, so raw top-N tables flagged a *healthy* cluster as full of "offenders/victims" — false alarms in prod.
- Gate added (the first real policy thresholds, as flags): `--min-samples` (default 100) drops small-sample p99 noise; `--runq-warn` (default 5ms) is the run-queue p99 a pod must exceed to count as a victim.
- The agent now prints a one-line `[OK] healthy` heartbeat when nothing crosses the gate, and the offender/victim tables **only** when at least one pod is genuinely starved.
- Fair share: the resolver pulls each container's CPU request from CRI (`ContainerStatus` shares → millicores); offenders are judged `within request` vs `OVER fair share` vs `best-effort`/`unattributed`.
- Live-validated: stable cluster stays silent across intervals; under `stress-ng` it trips with the hog at ~91% (unattributed) and real victims listed. Threshold is workload-dependent — tuned the default to the log2 bucket that separates healthy (~3ms) from contended (~6ms) here.

### Observability surfaces — Prometheus + sentinelctl (Phase 1 exit criteria)
- Refactored the agent to build one shared `report.Snapshot` per interval, published to stdout, Prometheus, and the CLI (so all three agree).
- `internal/server`: Prometheus `/metrics` (+ `/healthz`, `/readyz`) via a custom collector that emits from the latest snapshot at scrape time — per-pod series only for current offenders/victims, so cardinality is bounded and a healthy node emits just `sentinel_node_contended` + `sentinel_cgroups_observed`. Plus a unix-socket JSON server for the CLI.
- `cmd/sentinelctl`: `top` (live) and `status` (one-shot), reading the agent's socket. Pure Go, ships as a second binary (`build.sh` builds all `cmd/*`).
- Live-validated on the box: healthy → `node_contended 0` / `[OK] HEALTHY`; under stress → `node_contended 1`, per-pod intensity series (hog at 0.88), and full offender/victim tables in `sentinelctl`.

### Adaptive baseline + confidence (§7.5 steps 3–6)
- `internal/metrics/baseline.go` — per-cgroup EMA of run-queue p99 (a pod's learned "normal"), with warmup, freeze-while-anomalous (so a sustained spike isn't absorbed), and prune-on-churn. Pure Go, unit-tested.
- Victim logic: absolute floor stays the primary, restart-safe signal; once a pod's baseline is warm it's a victim only if *also* ≥ `--deviation`× its own normal — so always-slow pods stop being flagged for being themselves. Each victim reports its degradation (`xBASELINE`).
- Offender confidence (0–1): combines how far a pod exceeds its fair share with how badly victims degraded; gated by `--confidence`. Attribution line states the verdict; unattributed system hogs and within-request pods score low/none on purpose.
- Surfaced everywhere: stdout, `sentinelctl`, and new metrics (`sentinel_pod_runqueue_degradation`, `sentinel_pod_offender_confidence`, `sentinel_max_offender_confidence`).
- Live-validated: acceptance test still PASS; with a warm baseline, victims showed 12–105× their own normal under stress, and confidence stayed honest (max 6% → "alert only") because the hog was a system process. Flipped CONCEPTS.md ideas 1–4 to ✅.

### Phase 1 closeout — live cgroup watcher + overhead benchmark
- `internal/cgroup/watcher.go` — inotify (fsnotify) watch over the cgroup tree; debounced create/delete events trigger a resolver refresh, so new pods are resolved in ~0.5s instead of up to a minute. The periodic rescan (now 60s) stays as the safety net; refreshes are serialized with a mutex. Live, acceptance test still PASS.
- `overhead.sh` — measures agent CPU/RSS (idle + under stress) and BPF handler cost against the design §16 budget.
- **Measured on the box:** agent **0.09% of node CPU idle / 0.13% under stress** (budget < 1% ✅), **~42 MB RSS** (< 50 MB ✅). BPF handlers: wakeup ~417 ns, switch ~672 ns/event — above the design's ~200 ns because `sched_switch` now does two jobs (CPU-time + run-queue latency, each walking the cgroup hierarchy); userspace budget met with wide headroom, in-kernel cost is a known optimization target.

**Phase 1 (Foundation) is complete** — design §23 exit criteria met: BPF loads verifier-clean, run-queue latency attributed per pod, `sentinelctl top`/`status` works, overhead < 1% CPU. Plus we front-loaded the offender signal, the contention judgement layer, adaptive baselines, and confidence scoring.

## Phase 2 — Broader observers

### Disk-I/O observer (`blkio`)
- `internal/ebpf/bpf/blkio_monitor.bpf.c` — `block_rq_insert`/`block_rq_complete` (tp_btf); per-cgroup I/O latency histogram + throughput, attributed via the issuing cgroup captured at insert (buffered writeback attributes to the kernel/root cgroup — a documented limitation). `internal/ebpf/blkio.go` loader + reader; second bpf2go generation in the same package.
- **Generalized the judgement to N dimensions**: extracted a shared `victims()` (floor + baseline-deviation) used by both CPU run-queue latency and I/O latency; offenders per dimension (CPU by intensity vs request, I/O by disk throughput share); each with its own baseline + confidence + attribution. Healthy unless a pod is starved of CPU **or** disk I/O.
- Surfaced: stdout + sentinelctl now show per-dimension sections; new metrics (`sentinel_pod_io_bytes`, `sentinel_pod_io_latency_p99_microseconds`, `sentinel_pod_io_offender_confidence`, `sentinel_max_io_offender_confidence`). New flags `--io-warn`, `--min-ops`.
- Live-validated: blkio observer loads verifier-clean; under `dd` disk load the offender table shows the writer at 99.5% throughput share and I/O victims by latency, with honest attribution (system hog → no confident pod offender). CPU acceptance test still PASS; overhead unchanged. Proved the observer model generalizes beyond CPU.

### Network observer (`net`)
- `internal/ebpf/bpf/net_monitor.bpf.c` — `tp_btf/tcp_retransmit_skb` (victim: TCP retransmits) + `fentry/tcp_sendmsg` (offender: TX bytes). The hard part: network events fire in softirq context, so the cgroup is read from the **socket** (`sk->sk_cgrp_data.cgroup`), not `current`. `internal/ebpf/net.go` loader + reader; third bpf2go generation.
- **Generalized the victim core to scalars**: extracted `judgeVictim(baseline, floor, cg, value)` shared by all three dimensions (CPU run-queue p99, I/O latency p99, net retransmit count). Net offenders by TX-throughput share (shared `shareConfidence`); net victims by retransmit count + baseline. Healthy unless a pod is starved of CPU, disk I/O, **or** network.
- Surfaced: stdout + sentinelctl NETWORK section; new metrics (`sentinel_pod_net_tx_bytes`, `sentinel_pod_net_retransmits`, `sentinel_pod_net_offender_confidence`, `sentinel_max_net_offender_confidence`). New flags `--retrans-warn`, `--min-segs`.
- Live-validated: net observer loads verifier-clean; TX attributed per pod (apiserver/etcd) via the socket cgroup read; under induced loopback packet loss the victims table shows etcd/apiserver retransmitting (~100/interval) and apiserver as the top TX offender, honest attribution. CPU acceptance test still PASS. **All three contention dimensions now share one judgement.**

### Offender baselines — judge "who changed", not "who's biggest"
- Problem (caught in review): ranking offenders by raw throughput share perpetually blames the busiest *legitimate* infrastructure — the apiserver is always the top network talker, so it always looked like the offender. Network/disk have no Kubernetes "request" to define a fair share, so raw share was all we had.
- Fix: applied "learn each pod's normal" (Idea 1) to the **offender** side. New per-dimension usage baselines (CPU-ns, disk-bytes, net-TX-bytes), learned every interval (even when healthy), spikes frozen. Offender confidence is now `min(changed, big-enough, harm)`: deviation-above-its-own-normal **and** a meaningful resource share (≥25% → full) **and** real victim degradation. Warmup fallback: fair-share for CPU (instant from the request), "can't attribute yet" for disk/net.
- Insight: **contention is a *change*, so the culprit is whoever *changed*** — a steadily-busy pod that didn't change isn't the cause of *new* contention. The magnitude gate stops a near-zero pod's relative blip from reading as 100%.
- Live-validated (warm agent + induced loopback loss): apiserver/etcd, though top TX talkers (43%/17% share), now score **0% confidence** (at their normal volume); victims show real degradation (etcd 7.2×, apiserver 8.6× their own normal retransmit rate). In-memory baselines are restart-safe by design (absolute floor covers warmup; durable history lives in Prometheus/events).

## Phase 3 — the controller (in progress)

Sliced so each step ships: **(1) reporting + cluster view → (2) K8s Events → (3) NodeHealthPolicy CRD + decision engine → (4) remediation.**

### Slice 1 — agent→controller reporting + cluster view
- `cmd/controller` + `internal/controller`: a cluster-level aggregator. Agents POST their `report.Snapshot` to it; it holds the latest per-node state and prints a one-line cluster summary (`nodes=N healthy=H contended=C stale=S`) plus a headline per contended node. HTTP API: `POST /report`, `GET /status` (JSON), `GET /healthz`. Stale nodes (no report within `--stale-after`) are flagged DataGap. Observe-only — no decisions, no K8s API, no remediation yet.
- Transport: reuses the JSON `Snapshot` over HTTP (no protoc toolchain); the controller is **portable Go** (builds/tests on macOS). gRPC streaming is the design target for when we need backpressure + the reverse AgentDirective channel (slice 3+).
- Agent side: `--controller-addr` makes it POST each interval, best-effort — a controller outage never disrupts local detection (the agent stays self-contained). `report.Snapshot` gained `NodeName` (from `NODE_NAME` env or hostname).
- `build.sh` builds all three binaries (agent, controller, sentinelctl) automatically. Validated locally + on the box.

### Deployment artifacts
- `Dockerfile` (distroless, packages the prebuilt static binaries — image builds + imports into containerd, validated), `.dockerignore`, `deploy/` Kubernetes manifests (namespace, RBAC/ServiceAccounts, agent DaemonSet with BPF caps + hostPID + `/sys` + CRI-socket mounts, controller Deployment + Service — per design §6.8), and `DEPLOY.md` — a step-by-step guide for both the Kubernetes path and the known-good bare-binary/systemd path. (kubectl apply unverifiable on the test box: its API discovery is degraded — a cluster issue, not the manifests.)

### Up next
- Slice 2: controller gets a Kubernetes client and emits **Events** on contention (observe/alert).
- Slice 3: `NodeHealthPolicy` CRD + decision engine (modes observe/alert/enforce); likely migrate transport to gRPC.
- Slice 4: **remediation** (taint/cordon/evict) behind the confidence + eviction-safety gates — the last 🔜 in CONCEPTS.md.
