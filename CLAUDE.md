# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

node-sentinel is an eBPF-powered "noisy neighbor" detection & remediation operator for Kubernetes. It observes kernel-level resource **contention** (run-queue latency, block-I/O latency, NIC queueing) — not just usage — attributes it to specific pods with confidence scoring, and remediates via taint/cordon/evict under CRD-defined policy.

The full design is in [`docs/`](docs/):
- `node-sentinel-design-v0.3.md` — the authoritative design (HLD, LLD, CRDs, attribution, safety, phases).
- `node-sentinel-internals.md` — end-to-end dataflow traced with real numbers + scale analysis.

**Read the design docs before non-trivial work.** When in doubt about structure, naming, or behavior, the design doc wins.

## Hard constraints

- **eBPF runs on Linux only.** macOS cannot load/attach BPF programs. The kernel-facing code (`internal/ebpf`, `internal/cgroup`, `internal/agent`, `cmd/agent`) is `//go:build linux` and is built/run on a **remote Linux build host** (kernel ≥ 5.10 with BTF, cgroups v2). Only `internal/metrics` is portable and testable on any OS.
- **Package layout must follow design §7.2.1.** This was an explicit requirement: loader/observer/types split in `internal/ebpf`, agent lifecycle in `internal/agent` (thin `cmd/agent/main.go`), resolver in `internal/cgroup`, etc. Add files with the names the design uses; don't invent a parallel structure.

## Build & test

The portable unit tests run locally (incl. macOS):

```sh
go test ./internal/metrics/...                              # all metrics tests
go test ./internal/metrics/ -run TestPercentileSplit -v     # a single test
```

**Docker build (any OS — mac/win/linux, no local toolchain).** The whole eBPF+Go build runs inside Docker, so you don't need clang/libbpf/bpftool/Go installed. The BPF object is compiled once (it's `tp_btf`/`fentry`-only, so CPU-arch-independent and little-endian) and the static Go binaries cross-compile per arch with no emulation. See `Dockerfile` + `docker-build.sh`:

```sh
./docker-build.sh binaries     # -> bin/linux_amd64/{agent,controller,sentinelctl} + bin/linux_arm64/...
./docker-build.sh image        # -> node-sentinel:dev, host arch, loaded into docker
./docker-build.sh image --push -t <registry>/node-sentinel:<tag>   # multi-arch (amd64+arm64) manifest
```

This works because `internal/ebpf/bpf/vmlinux.h` is **committed** (not host-dumped): CO-RE relocates field offsets at load time, so one checked-in header serves every kernel/arch. Regenerate it only when adding probes that read new kernel structs (`make vmlinux`, or `./docker-build.sh vmlinux`).

**Native build on a Linux host** (fastest inner loop if you already have the toolchain: Go ≥ 1.25, clang/LLVM, libbpf-dev, bpftool, make). The Makefile flow:

```sh
make setup      # one-time: go get github.com/cilium/ebpf@latest && go mod tidy
make vmlinux    # (re)dump kernel BTF -> internal/ebpf/bpf/vmlinux.h (the committed CO-RE header)
make generate   # bpf2go: compile sched_monitor.bpf.c + generate Go bindings (needs clang)
make build      # CGO_ENABLED=0 go build -o bin/agent ./cmd/agent
sudo ./bin/agent --interval 5s --top 12                     # run (needs root / BPF caps)
make test       # portable unit tests
```

`go build ./...` / `go test ./...` from macOS will fail on the linux-tagged packages — that's expected; build those on the host.

### Remote-host dev loop

Edit locally, sync to the host, build there:

```sh
rsync -az --delete --exclude '.git' --exclude 'bin' ./ <user>@<host>:~/node-sentinel/
# on host: export PATH=$PATH:/usr/local/go/bin && make vmlinux generate build
```

**Gotcha:** `rsync --delete` wipes the host's bpf2go output (`internal/ebpf/sched_bpfel.go`, `*.o`) because those stay gitignored and don't exist locally. Re-run `make generate` after a `--delete` sync, or `--exclude` them. (`vmlinux.h` is now committed, so it survives the sync — only the bpf2go `.go`/`.o` files are regenerated.)

## Architecture (big picture)

Strictly forward-only pipeline; each stage never calls back into an earlier one:

```
eBPF (kernel)    → collects signals      (per-cgroup log2 histograms, per-CPU maps)
Agent (Go)       → aggregates + detects  (merge per-CPU, percentiles, resolve to pods)
Controller (Go)  → decides + acts        (attribution, confidence gate, taint/cordon/evict)  [not built yet]
Kubernetes       → enforces actions
```

Current code is **Phase 1 (Foundation)** — the agent half only. Key pieces and how they fit:

- `internal/ebpf/bpf/sched_monitor.bpf.c` — CO-RE BPF (tp_btf raw tracepoints). `sched_wakeup`/`sched_wakeup_new` stamp wakeup time per pid; `sched_switch` computes `now − wakeup` for the incoming task = **run-queue latency**, bucketed into a per-cgroup log2 histogram (microseconds) in a `PERCPU_HASH`.
- `internal/ebpf/{loader,sched,types}.go` — load+attach, and read maps with a **read-and-delete** snapshot per interval (design §7.2.3): sum the per-CPU copies, then delete the key.
- `internal/metrics/histogram.go` — log2 histogram → percentile estimate (bucket midpoint = `2^i * 1.5`). Pure Go, unit-tested.
- `internal/cgroup/resolver.go` — maps `cgroup_id` → `namespace/pod/container` (design §7.4). The `cgroup_id` eBPF reads **equals the cgroup directory inode** (exact on kernels ≥ 5.5 with 64-bit inodes), so it scans the cgroup tree for container scopes (`cri-containerd-<id>.scope` etc.), gets each inode, and joins against containerd's CRI `ListContainers` labels. Cgroups with no CRI container (system slices, pause sandboxes) resolve to `unknown` and are **never attributed** — a deliberate safety rule.
- `internal/agent/{agent,config}.go` + `cmd/agent/main.go` — lifecycle: load observer, periodic map read + pod-resolver refresh, print worst cgroups by p99.

## Conventions / things to know

- **Run-queue latency is a victim-side signal.** It measures how long woken tasks wait for a CPU — so it lights up on the *victims* of contention, not the offender. A CPU-hog pod rarely sleeps, so it generates few wakeup→switch events. Identifying the *offender* needs the separate per-cgroup CPU-time **intensity** signal (design §7.5 step 2), which is not built yet. Don't interpret a high-p99 pod as the culprit.
- BPF C is restricted C compiled to bytecode. `vmlinux.h` is **committed** (one CO-RE header for all kernels/arches — see Build & test); regenerate only when a new probe reads new structs. The kernel struct reads most likely to break across kernels: `BPF_CORE_READ(next, cgroups, dfl_cgrp, kn, id)` and the `tp_btf/sched_switch` argument signature.
- Module path: `github.com/codecrafted007/node-sentinel`.
- Generated bpf2go files (`*_bpfel.go`, `*.o`) are gitignored and regenerated by the build (native `make generate` or the Docker builder). `vmlinux.h` is the exception — committed, so the Docker build is hermetic.
- `CONCEPTS.md` explains what the system does and how it decides (offender/victim, baseline, confidence) in plain-English analogies — it marks each idea ✅ built vs 🔜 planned, so keep those tags accurate as features land.
- `HOW.md` is the onboarding explainer for how the eBPF probe is compiled, embedded (bpf2go + `go:embed`), loaded, and attached — point new contributors there.
- See `PROGRESS.md` for a running log of completed work.
