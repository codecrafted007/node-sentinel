# Progress Log

A running record of completed work, newest phase first. Roadmap lives in design §23.

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
- Module path set to `github.com/codecrafted007/node-sentinal`; git remote `origin` → `git@github.com:codecrafted007/node-sentinal.git`.
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

## Up next — finish observers + the controller (Phases 2–3)

- Network observer (`net`): TCP retransmits, packet drops, NIC queue latency — same treatment, last observer before the controller.
- Build the **controller**: gRPC agent→controller stream, `NodeHealthPolicy` CRD, decision engine, and remediation (taint/cordon/evict) that *acts* on high-confidence offenders — the last 🔜 in CONCEPTS.md.
