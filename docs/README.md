# Documentation index

Start here, then follow the path that fits what you need.

## Understand it
- [`CONCEPTS.md`](CONCEPTS.md) — what node-sentinel does and how it decides, in plain English (offender/victim, baselines, confidence). Tagged ✅ built / 🔜 planned.
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — three diagrams: where it runs, how data flows, inside one agent.
- [`HOW.md`](HOW.md) — how the eBPF probe is compiled, embedded (bpf2go + `go:embed`), loaded, and attached.
- [`DEEPDIVE.md`](DEEPDIVE.md) — **the "GOD book."** Everything from foundation to top in one read: why it exists, what eBPF is, how we capture the data, cgroups/PIDs internals, C↔Go embedding, how Go reads the maps, and the full decision model — with diagrams, equations, and a line-by-line trace. Start here if you want to understand *every* number the agent prints.
- [`sim/temporal-correlation.html`](sim/temporal-correlation.html) — two interactive toys (open in any browser, no build): why a coarse scrape hides short bursts, and how shape-not-magnitude correlation pins the offender. Models the 🔜 sub-interval correlation feature.

## Run it
- [`DEPLOY.md`](DEPLOY.md) — Kubernetes manifests and bare-binary/systemd, step by step.
- [`HELPERCOMMANDS.md`](HELPERCOMMANDS.md) — every command (build, run, deploy, inspect, validate) with real flags and defaults.
- [`DETECTION-DEMO.md`](DETECTION-DEMO.md) — a reproducible walkthrough of catching a noisy neighbour on a live cluster, with real output.

## Design (authoritative)
- [`node-sentinel-design-v0.3.md`](node-sentinel-design-v0.3.md) — HLD, LLD, CRDs, attribution, safety, phases.
- [`node-sentinel-internals.md`](node-sentinel-internals.md) — end-to-end dataflow traced with real numbers + scale analysis.

## Project history
- [`PROGRESS.md`](PROGRESS.md) — running log of completed work.

---

Back to the [project README](../README.md).
