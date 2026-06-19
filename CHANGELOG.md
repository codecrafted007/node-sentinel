# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project is
pre-1.0 (no released versions yet). For the detailed slice-by-slice log, see
[`docs/PROGRESS.md`](docs/PROGRESS.md).

## [Unreleased]

### Added
- **Detection (agent), all three dimensions:** eBPF observers for CPU scheduling
  (run-queue latency + CPU intensity), disk I/O latency, and network (NIC queueing /
  TCP retransmits).
- **Honest pod attribution:** cgroup → Kubernetes pod via the CRI socket; cgroups
  with no CRI container resolve to `unknown` and are never attributed.
- **Judgement & confidence:** learned per-pod baselines, "unusual *and* actually bad"
  gating, and offender confidence scoring (victim and offender sides). Low-confidence
  findings are alert-only.
- **Cluster controller (observe-only):** aggregates per-node reports into a
  cluster-wide view (`/status`, `/healthz`).
- **Observability:** Prometheus `/metrics`, the `sentinelctl` live CLI (`top`/`status`),
  and measured < 1% CPU overhead.
- **Any-OS Docker build:** `scripts/docker-build.sh` produces multi-arch
  (amd64+arm64) binaries and image with no local toolchain; committed CO-RE
  `vmlinux.h` makes the build hermetic.
- **Kubernetes manifests** (`deploy/`) and a single-node test overlay.
- **Docs:** concepts, architecture diagrams, how-it-works, deploy guide, command
  reference, and a live-cluster detection walkthrough (`docs/`).

### Changed
- Repository restructured to a conventional layout: narrative docs under `docs/`,
  shell scripts under `scripts/`, with the Makefile as the front door.

### Roadmap (not yet built)
- Controller-emitted Kubernetes Events.
- `NodeHealthPolicy` CRD + decision engine.
- Remediation (taint / cordon / evict) under confidence gates.

[Unreleased]: https://github.com/codecrafted007/node-sentinel/commits/main
