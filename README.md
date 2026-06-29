# node-sentinel

**eBPF-powered noisy-neighbor detection & remediation operator for Kubernetes.**

Kubernetes shares node resources (CPU run queues, disk I/O queues, NIC queues) across pods, but the scheduler can't see contention at those boundaries. When one pod saturates a shared resource, its neighbors degrade silently. node-sentinel observes the contention *inside the kernel* with eBPF, attributes it to specific pods, and remediates under operator-defined policy.

![node-sentinel demo](docs/demo/node-sentinel-demo.gif)

> *Detect → attribute (with a confidence score) → throttle → restore. The host agent flags the offender (and leaves the merely-busy pods alone — busy ≠ guilty); a `NodeHealthPolicy` in `enforce` mode has the controller throttle it in place via `/resize` and restore it after the window. Representative output; see [`DEPLOY-AND-TEST.md`](docs/DEPLOY-AND-TEST.md) to reproduce.*

**New here?** Start with [`CONCEPTS.md`](docs/CONCEPTS.md) (what it does & how it decides, in plain English), then [`ARCHITECTURE.md`](docs/ARCHITECTURE.md) (three diagrams: where it runs, how data flows, inside one agent) and [`HOW.md`](docs/HOW.md) (how the eBPF probe is built, embedded, and run). **Want the whole thing end to end?** → [`DEEPDIVE.md`](docs/DEEPDIVE.md), the "GOD book": foundation-to-top in one read (eBPF, cgroups/PIDs, C↔Go embedding, map reading, and the full decision model — diagrams, equations, line-by-line trace). **Want to run it?** → [`DEPLOY.md`](docs/DEPLOY.md) (Kubernetes manifests + bare-binary/systemd, step by step) · every command in one place: [`HELPERCOMMANDS.md`](docs/HELPERCOMMANDS.md) · see it catch a noisy neighbour on a live cluster: [`DETECTION-DEMO.md`](docs/DETECTION-DEMO.md). Full design: [`docs/node-sentinel-design-v0.3.md`](docs/node-sentinel-design-v0.3.md) · dataflow & scale: [`docs/node-sentinel-internals.md`](docs/node-sentinel-internals.md) · progress log: [`PROGRESS.md`](docs/PROGRESS.md).

---

## Status — detection works; remediation is roadmap

At a glance — what's real today vs. what the design still promises:

| Capability | State |
|---|---|
| Per-node agent: eBPF observers for **CPU, disk I/O, network** | ✅ built |
| cgroup → Kubernetes pod attribution (via CRI) | ✅ built |
| Contention **judgement** — quiet unless genuinely contended | ✅ built |
| Learned per-pod **baselines** + **confidence** scoring (victim *and* offender side) | ✅ built |
| Observability: Prometheus `/metrics`, `sentinelctl` CLI, <1% CPU overhead (measured) | ✅ built |
| Cluster **controller**: aggregates per-node reports, cluster view (observe-only) | ✅ built |
| Controller emits Kubernetes **Events** | 🔜 roadmap |
| **NodeHealthPolicy** CRD + decision engine | 🔜 roadmap |
| **Remediation** (taint / cordon / evict) under confidence gates | 🔜 roadmap |

In short: node-sentinel today is a **production-quiet, multi-dimensional contention _detector_** with honest pod attribution. The **_remediation_** half of the design (the controller acting on offenders) is not built yet — see the roadmap in design §23 and the slice plan in [`PROGRESS.md`](docs/PROGRESS.md).

The per-node **agent** works end-to-end: it loads eBPF observers for **CPU scheduling, disk I/O, and network**, resolves cgroups to Kubernetes pods, and — crucially — **stays quiet unless the node is genuinely contended**. A stable cluster logs one line per interval; when a pod is actually starved (of CPU, disk I/O, or network) it prints per-dimension **offenders** (who's over-using the resource) and **victims** (who's suffering), each judged against a learned baseline with a confidence score. A cluster-level **controller** aggregates every node's report into one view — observe-only for now; it does not yet take action.

Stable node — just a heartbeat:

```
12:30:05  [OK] healthy — no CPU contention (no pod above run-queue p99 5ms with >=100 samples; 77 cgroups seen)
```

Under real contention (a CPU hog running, baseline warmed up):

```
01:47:08  [!] CPU CONTENTION — 7 pod(s) starved on the run queue
  attribution: low confidence (6% < 70% threshold) — alert only
  OFFENDERS — by CPU time
  POD                                    CPU_MS INTENSITY  REQ_m CONFIDENCE  VERDICT
  system(cg:2660936)                      53673     89.9%      -          —  system / unattributed
  default/kafka-0/kafka                    1720      2.9%    250         0%  within request (14.5%)
  VICTIMS — by run-queue latency (p99 >= 5ms, >=100 samples)
  POD                              RUNQ_P50_US  RUNQ_P99_US xBASELINE     EVENTS
  default/udpx-p6t9s/udpx                   48         6144    104.8x      42178
  kube-system/calico-…/controllers          24        24576     26.6x        226
```

> Two signals from one tracepoint: **CPU intensity** (offender — share of CPU consumed, judged against the pod's request) and **run-queue latency** (victim — how long it waited). On top of those: each victim's **xBASELINE** shows how far it's degraded from its *own* learned normal, and each offender gets a **confidence** score. The `attribution` line is the honest verdict — here it refuses to blame a pod because the real hog is a system process. See [`CONCEPTS.md`](docs/CONCEPTS.md) for the plain-English model. Still to come: *acting* on high-confidence offenders (the controller).

---

## Build with Docker (any OS — the easy path)

You don't need the eBPF toolchain (clang/libbpf/bpftool/Go) on your machine — just Docker. The whole build runs in a container, so a dev on **macOS, Windows, or Linux** can produce the Linux binaries and the image:

```sh
git clone git@github.com:codecrafted007/node-sentinel.git
cd node-sentinel

./scripts/docker-build.sh binaries     # cross-arch static binaries -> bin/linux_amd64/... + bin/linux_arm64/...
./scripts/docker-build.sh image        # node-sentinel:dev for your host arch, loaded into docker
./scripts/docker-build.sh image --push -t <registry>/node-sentinel:<tag>   # multi-arch (amd64+arm64) manifest
```

It's fast: the BPF object is compiled **once** (our probes are `tp_btf`/`fentry`-only, so the bytecode is CPU-arch-independent), and the `CGO_ENABLED=0` Go binaries cross-compile per arch with no QEMU emulation. The CO-RE header `internal/ebpf/bpf/vmlinux.h` is committed, so the build is fully offline/hermetic — one header relocates against any running kernel ≥ 5.10 at load time. (The agent still only *runs* on Linux; see below.)

Then copy the right binary onto a Linux node and run it, or deploy the image — see [`DEPLOY.md`](docs/DEPLOY.md).

## Why you still need a Linux box (to *run* it)

eBPF only loads on Linux. **macOS / Windows can build (above) and run the portable unit tests, but cannot run the agent** — loading/attaching BPF needs a Linux kernel. To run the agent you need a Linux host (or the Docker build path above plus a Linux node to deploy to).

**Build host requirements:**
- Linux kernel **≥ 5.10** with BTF (`/sys/kernel/btf/vmlinux` exists) and **cgroups v2** (`stat -fc %T /sys/fs/cgroup` → `cgroup2fs`)
- A container runtime exposing CRI (containerd or CRI-O) for pod resolution
- Toolchain: **Go ≥ 1.25, clang/LLVM, libbpf-dev, bpftool, make**
- Root (or `CAP_BPF` + `CAP_PERFMON` + `CAP_SYS_RESOURCE`) to load BPF

Install the toolchain on Ubuntu 22.04:

```sh
sudo apt-get update
sudo apt-get install -y clang llvm libbpf-dev make linux-tools-common linux-tools-$(uname -r)
# Go (apt's is too old): grab the official tarball
curl -fsSL https://go.dev/dl/go1.25.6.linux-amd64.tar.gz | sudo tar -C /usr/local -xzf -
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.profile && source ~/.profile
```

---

## Native build (on a Linux host with the toolchain)

Faster inner loop if you already have clang/libbpf/bpftool/Go installed:

```sh
git clone git@github.com:codecrafted007/node-sentinel.git
cd node-sentinel

make setup      # one-time: fetch Go deps (cilium/ebpf, cri-api, grpc)
make generate   # compile the BPF C + generate Go bindings (needs clang; vmlinux.h is committed)
make build      # -> bin/agent
# make vmlinux  # optional: re-dump this kernel's BTF over the committed header (only if adding probes)

sudo ./bin/agent --interval 5s --top 12
```

`Ctrl-C` to stop. Flags:

| flag | default | meaning |
|------|---------|---------|
| `--interval` | `5s` | how often maps are read and a report is printed |
| `--top` | `20` | how many cgroups to show per table |
| `--runq-warn` | `5ms` | run-queue p99 a pod must exceed to count as a victim (the absolute floor) |
| `--min-samples` | `100` | run-queue samples a pod needs before its p99 is trusted (kills small-sample noise) |
| `--deviation` | `3.0` | once a pod's baseline is warm, how many × its *own* normal p99 counts as a victim |
| `--confidence` | `0.7` | offender confidence needed to name a pod the noisy neighbour (else alert-only) |
| `--io-warn` | `20ms` | disk I/O p99 latency a pod must exceed to count as an I/O victim |
| `--min-ops` | `20` | completed I/O requests a pod needs before its I/O p99 is trusted |
| `--retrans-warn` | `10` | TCP retransmits/interval a pod must exceed to count as a network victim |
| `--min-segs` | `50` | sendmsg calls a pod needs before its retransmits are judged |
| `--metrics-addr` | `:2112` | Prometheus `/metrics` listen address (empty to disable) |
| `--local-socket` | `/var/run/sentinel/agent.sock` | unix socket for `sentinelctl` (empty to disable) |
| `--cri-socket` | `unix:///run/containerd/containerd.sock` | CRI endpoint for pod resolution |
| `--cgroup-root` | `/sys/fs/cgroup/kubepods.slice` | cgroup subtree scanned for pods |

`--runq-warn` and `--min-samples` are the contention gate: a stable node prints a single `[OK] healthy` line, and the offender/victim tables appear **only** when a pod is genuinely starved of CPU. The right `--runq-warn` is workload-dependent — latency-sensitive services warrant a lower value; batch nodes a higher one. (Per-workload baselines that adapt this automatically are the next step; design §7.5.)

If the CRI socket is unreachable, the agent still runs and prints raw cgroup IDs (pod resolution is best-effort, never a hard dependency).

## Observability

The agent publishes the same judgement to three places each interval: stdout, a Prometheus endpoint, and a unix socket for the CLI.

**Prometheus** — scrape `http://<node>:2112/metrics` (also `/healthz`, `/readyz`):

| metric | type | labels | meaning |
|--------|------|--------|---------|
| `sentinel_node_contended` | gauge | — | 1 if the node is CPU-contended, else 0 |
| `sentinel_cgroups_observed` | gauge | — | cgroups seen in the last interval |
| `sentinel_pod_cpu_intensity_ratio` | gauge | `pod` | offender pod's share of CPU consumed (0–1) |
| `sentinel_pod_cpu_milliseconds` | gauge | `pod` | offender pod's on-CPU ms this interval |
| `sentinel_pod_runqueue_p99_microseconds` | gauge | `pod` | victim pod's run-queue p99 |
| `sentinel_pod_runqueue_p50_microseconds` | gauge | `pod` | victim pod's run-queue p50 |
| `sentinel_pod_runqueue_degradation` | gauge | `pod` | victim pod's p99 ÷ its own learned baseline |
| `sentinel_pod_offender_confidence` | gauge | `pod` | confidence (0–1) this pod is the noisy neighbour |
| `sentinel_max_offender_confidence` | gauge | — | highest CPU offender confidence this interval (−1 if none attributable) |
| `sentinel_pod_io_bytes` | gauge | `pod` | offender pod's disk bytes this interval |
| `sentinel_pod_io_latency_p99_microseconds` | gauge | `pod` | I/O-victim pod's disk latency p99 |
| `sentinel_pod_io_offender_confidence` | gauge | `pod` | confidence (0–1) this pod is the disk noisy neighbour |
| `sentinel_max_io_offender_confidence` | gauge | — | highest disk-I/O offender confidence this interval |
| `sentinel_pod_net_tx_bytes` | gauge | `pod` | offender pod's TCP TX bytes this interval |
| `sentinel_pod_net_retransmits` | gauge | `pod` | victim pod's TCP retransmits this interval |
| `sentinel_pod_net_offender_confidence` | gauge | `pod` | confidence (0–1) this pod is the network noisy neighbour |
| `sentinel_max_net_offender_confidence` | gauge | — | highest network offender confidence this interval |

Per-pod series are emitted only for the pods currently in the offender/victim lists, so cardinality is bounded and a healthy node emits just the two node-level gauges. `sentinel_node_contended` is the one to alert on; `sentinel_max_offender_confidence` tells you whether a specific pod can be blamed.

**`sentinelctl`** — the on-node CLI (read-only, over the agent's unix socket):

```sh
sentinelctl top       # live, refreshing view (default)
sentinelctl status    # one-shot snapshot
# --socket /var/run/sentinel/agent.sock   --interval 2s
```

In-cluster you'd `kubectl exec` into the agent pod and run `sentinelctl top` for an htop-style view of who's burning CPU and who's waiting for it.

---

## Developing from macOS (edit local, build remote)

```sh
# 1. run the portable tests locally
go test ./internal/metrics/...

# 2. sync to the host and build there
rsync -az --delete --exclude '.git' --exclude 'bin' ./ <user>@<host>:~/node-sentinel/
ssh <user>@<host> 'export PATH=$PATH:/usr/local/go/bin && cd ~/node-sentinel && make vmlinux generate build && sudo ./bin/agent'
```

**Heads up:** `rsync --delete` removes the host's generated files (`sched_bpfel.go`, `*.o`, `vmlinux.h`) since they're gitignored and absent locally. Re-run `make vmlinux generate` after syncing (or add `--exclude` for them).

## Stress testing & validation

`stress-test.sh` is an **acceptance test** for the contention detector. It verifies the property that matters in production — *quiet when healthy, loud only under real contention* — across three phases, and prints **PASS/FAIL** (exit non-zero on failure, so it can gate CI):

```sh
sudo apt-get install -y stress-ng        # one-time prerequisite
./scripts/build.sh                               # ensure bin/agent exists
sudo ./scripts/stress-test.sh                     # runs the test, prints PASS/FAIL
# options: --workers N (default 4×nproc)  --duration S  --interval 5s  --top 10
```

| phase | what it does | passes when |
|-------|--------------|-------------|
| **1 — baseline** | runs the agent on the idle node | reports `[OK] healthy`, **no** contention |
| **2 — under stress** | injects `stress-ng` CPU hogs | reports `[!] CPU CONTENTION` |
| **3 — recovery** | stops the load, waits, re-runs | returns to `[OK] healthy` |

```
RESULT: PASS — detector stays quiet when healthy and fires under stress
```

The agent's output is shown for each phase so you can read the actual numbers. Two things to understand:

- **Offenders vs. victims are different tables.** Under contention, the hog tops **OFFENDERS** (high CPU intensity, judged against its CPU request); the starved pods show in **VICTIMS** (high run-queue latency). The hog barely appears in VICTIMS — CPU hogs rarely sleep, so they emit few `wakeup→switch` events — which is why we need both signals.
- **Pod names come from the CRI socket, not `kubectl`.** Resolution works even if the kube API server is degraded — the resolver reads containerd directly (design §7.4).

If **PHASE 1 fails** (baseline flags contention), the node is genuinely busy or `--runq-warn` is set too low for this workload — raise it. If **PHASE 2 fails**, the stress isn't crossing the threshold — lower `--runq-warn` or add `--workers`.

Prefer to drive it by hand? The core of what the script does:

```sh
sudo systemd-run --unit=ns-stress --collect stress-ng --cpu $(( $(nproc) * 4 )) --timeout 45s
sudo ./bin/agent --interval 5s --top 12     # watch p50/p99 climb
sudo systemctl stop ns-stress               # watch them recover
```

### Overhead

`sudo ./scripts/overhead.sh` measures the agent against the budget (design §16): userspace CPU and RSS (idle and under stress) plus per-event BPF handler cost. On a 12-core node it measured **~0.1% of node CPU and ~42 MB RSS** — well within the < 1% CPU / < 50 MB budget. (The BPF handlers run ~400–700 ns/event because `sched_switch` does both CPU-time and run-queue-latency accounting.)

---

## Project layout (follows design §7.2.1)

```
cmd/agent/            agent entrypoint: flags + signal handling
cmd/sentinelctl/      on-node CLI (top / status)
cmd/controller/       cluster controller (aggregates per-node reports)  [Phase 3]
internal/agent/       lifecycle (agent.go) + config (config.go)
internal/ebpf/        loader.go, observers (sched/blkio/net), types.go, bpf/*.bpf.c
internal/cgroup/      resolver.go (cgroup_id -> pod via CRI) + watcher.go (inotify live updates)
internal/metrics/     histogram.go + baseline.go (learned normals)  — portable, tested
internal/report/      shared snapshot type (portable)
internal/server/      Prometheus /metrics + sentinelctl unix socket (portable)
internal/controller/  cluster aggregator (portable)  [Phase 3]
deploy/               Kubernetes manifests (namespace, rbac, agent, controller)
scripts/              build.sh, docker-build.sh, stress-test.sh, overhead.sh
docs/                 design, internals, concepts, architecture, how, deploy, commands, demo
```

The controller is **observe-only** today: run it anywhere, point agents at it with `--controller-addr http://<host>:<port>`, and it prints a cluster-wide contention summary. Kubernetes Events, a `NodeHealthPolicy` CRD, and remediation are the next slices.

## Build targets

| target | what it does |
|--------|--------------|
| `make setup` | fetch Go dependencies (run once) |
| `make vmlinux` | dump kernel BTF to a CO-RE header |
| `make generate` | compile BPF C + generate Go bindings (bpf2go, needs clang) |
| `make build` | build `bin/agent` |
| `make agent` | build + run with sudo |
| `make test` | portable unit tests (any OS) |
| `make stress` | acceptance test — quiet-when-healthy, loud-under-contention (Linux, root) |
| `make overhead` | measure agent CPU/RSS vs. the design §16 budget (Linux, root) |
| `make docker-binaries` | cross-arch static binaries via Docker (any OS) |
| `make docker-image` | build the image via Docker (any OS) |
| `make clean` | remove `bin/` and generated bindings |

## Troubleshooting

- **`undefined: schedObjects` / `loadSchedObjects`** — generated bindings missing; run `make vmlinux generate` (common after an `rsync --delete`).
- **agent exits at load (verifier/BTF error)** — confirm BTF (`ls /sys/kernel/btf/vmlinux`) and kernel ≥ 5.10; the `cgroups…kn.id` read and `tp_btf/sched_switch` arg signature are the usual cross-kernel sore spots.
- **all pods show as `system(cg:N)`** — CRI socket wrong/unreachable; check `--cri-socket` and that the agent runs as root.
- **`apt` "no longer has a Release file" / "unmet dependencies"** — transient mirror or a half-finished upgrade; `sudo apt-get -o Acquire::Retries=5 update` then `sudo apt-get --fix-broken install`.

## License

[Apache License 2.0](LICENSE) © 2026 Brajesh Pant.
