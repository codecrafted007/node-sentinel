# node-sentinel

**eBPF-powered noisy-neighbor detection & remediation operator for Kubernetes.**

Kubernetes shares node resources (CPU run queues, disk I/O queues, NIC queues) across pods, but the scheduler can't see contention at those boundaries. When one pod saturates a shared resource, its neighbors degrade silently. node-sentinel observes the contention *inside the kernel* with eBPF, attributes it to specific pods, and (in later phases) remediates under operator-defined policy.

Full design: [`docs/node-sentinel-design-v0.3.md`](docs/node-sentinel-design-v0.3.md) · dataflow & scale: [`docs/node-sentinel-internals.md`](docs/node-sentinel-internals.md) · **new here? start with [`HOW.md`](HOW.md)** (how the eBPF probe is built, embedded, and run) · progress log: [`PROGRESS.md`](PROGRESS.md).

---

## Status — Phase 1 (Foundation), in progress

The per-node **agent** works end-to-end: it loads an eBPF scheduler observer, measures per-cgroup run-queue latency, resolves cgroups to Kubernetes pods, and prints live percentiles. The controller (decision/attribution/remediation) is not built yet — see the roadmap in design §23.

Sample output (running against a live cluster):

```
pod resolver: 73 containers mapped
node-sentinel agent: sched observer attached, reading every 5s

POD (namespace/pod/container)                 RUNQ_P50_US  RUNQ_P99_US     EVENTS
default/nascontroller-9nll5/nascontroller               2         6144         57
kube-system/calico-kube-controllers-b8cb7df…            2         1536        174
kube-system/kube-proxy-gnhtq/kube-proxy                 3          192         34
system(cg:1448331)                                      2         1536       1317
```

> Run-queue latency is the **victim-side** signal — it shows pods *waiting* for CPU, not the hog causing it. Offender attribution (CPU-time intensity) is the next piece.

---

## Why you need a Linux box

eBPF only loads on Linux. **macOS / Windows cannot run the agent** — they can edit code and run the portable unit tests, but the kernel-facing build happens on a Linux host.

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

## Quick start (on the Linux host)

```sh
git clone git@github.com:codecrafted007/node-sentinal.git
cd node-sentinal

make setup      # one-time: fetch Go deps (cilium/ebpf, cri-api, grpc)
make vmlinux    # dump this kernel's BTF -> internal/ebpf/bpf/vmlinux.h
make generate   # compile the BPF C + generate Go bindings (needs clang)
make build      # -> bin/agent

sudo ./bin/agent --interval 5s --top 12
```

`Ctrl-C` to stop. Flags:

| flag | default | meaning |
|------|---------|---------|
| `--interval` | `5s` | how often maps are read and a report is printed |
| `--top` | `20` | how many cgroups to show (sorted by run-queue p99) |
| `--cri-socket` | `unix:///run/containerd/containerd.sock` | CRI endpoint for pod resolution |
| `--cgroup-root` | `/sys/fs/cgroup/kubepods.slice` | cgroup subtree scanned for pods |

If the CRI socket is unreachable, the agent still runs and prints raw cgroup IDs (pod resolution is best-effort, never a hard dependency).

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

To prove the agent actually detects contention (and to validate any new observer), use the `stress-test.sh` helper. It runs **baseline → inject CPU hogs → measure → stop → recovery** and prints the agent output at each phase:

```sh
sudo apt-get install -y stress-ng        # one-time prerequisite
./build.sh                               # ensure bin/agent exists
sudo ./stress-test.sh                     # full baseline/inject/measure/recover run
# options: --workers N (default 4×nproc)  --duration S  --interval 5s  --top 10
```

**How to read the result** — run-queue latency per pod should move like this:

| phase | run-queue p50 | p99 |
|-------|---------------|-----|
| baseline (idle) | a few µs | mostly low |
| under load | **hundreds of µs** (~100× higher) | multi-millisecond |
| after stop (≈4s) | back to a few µs | back to baseline |

Two things to understand about the output:

- **It shows victims, not the culprit.** Run-queue latency measures pods *waiting* for a CPU, so the pods that light up are the ones being starved. The hog cgroup itself barely appears — CPU hogs rarely sleep, so they emit few `wakeup→switch` events. Naming the offender needs the CPU-time **intensity** signal (design §7.5 step 2), not yet implemented.
- **Pod names come from the CRI socket, not `kubectl`.** Resolution works even if the kube API server is degraded — the resolver reads containerd directly (design §7.4).

Prefer to drive it by hand? The core of what the script does:

```sh
sudo systemd-run --unit=ns-stress --collect stress-ng --cpu $(( $(nproc) * 4 )) --timeout 45s
sudo ./bin/agent --interval 5s --top 12     # watch p50/p99 climb
sudo systemctl stop ns-stress               # watch them recover
```

---

## Project layout (follows design §7.2.1)

```
cmd/agent/            agent entrypoint: flags + signal handling
internal/agent/       lifecycle (agent.go) + config (config.go)
internal/ebpf/        loader.go, sched.go (reader), types.go, bpf/*.bpf.c
internal/cgroup/      resolver.go — cgroup_id -> pod (via CRI)
internal/metrics/     histogram.go — log2 histogram -> percentiles (portable, tested)
docs/                 design + internals
```

## Build targets

| target | what it does |
|--------|--------------|
| `make setup` | fetch Go dependencies (run once) |
| `make vmlinux` | dump kernel BTF to a CO-RE header |
| `make generate` | compile BPF C + generate Go bindings (bpf2go, needs clang) |
| `make build` | build `bin/agent` |
| `make agent` | build + run with sudo |
| `make test` | portable unit tests (any OS) |
| `make clean` | remove `bin/` and generated bindings |

## Troubleshooting

- **`undefined: schedObjects` / `loadSchedObjects`** — generated bindings missing; run `make vmlinux generate` (common after an `rsync --delete`).
- **agent exits at load (verifier/BTF error)** — confirm BTF (`ls /sys/kernel/btf/vmlinux`) and kernel ≥ 5.10; the `cgroups…kn.id` read and `tp_btf/sched_switch` arg signature are the usual cross-kernel sore spots.
- **all pods show as `system(cg:N)`** — CRI socket wrong/unreachable; check `--cri-socket` and that the agent runs as root.
- **`apt` "no longer has a Release file" / "unmet dependencies"** — transient mirror or a half-finished upgrade; `sudo apt-get -o Acquire::Retries=5 update` then `sudo apt-get --fix-broken install`.
