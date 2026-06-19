# Contributing to node-sentinel

Thanks for your interest! This guide covers the practical setup. The design is authoritative — **read [`docs/node-sentinel-design-v0.3.md`](docs/node-sentinel-design-v0.3.md) before non-trivial work**, and see [`docs/CONCEPTS.md`](docs/CONCEPTS.md) / [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the model.

## Ground rules

- **eBPF runs on Linux only.** macOS/Windows can build (via Docker) and run the portable unit tests, but cannot load/attach BPF. The kernel-facing packages (`internal/ebpf`, `internal/cgroup`, `internal/agent`, `cmd/agent`) are `//go:build linux`.
- **Package layout follows design §7.2.1.** Add files with the names the design uses; don't invent a parallel structure.
- **The design doc wins.** When in doubt about structure, naming, or behaviour, match the design.

## Build & test

No local toolchain needed — the whole eBPF + Go build runs in Docker:

```sh
make docker-binaries     # cross-arch static binaries -> bin/<os>_<arch>/
make docker-image        # node-sentinel:dev (host arch)
```

Native (on a Linux host with Go ≥ 1.25, clang/LLVM, libbpf-dev, bpftool, make):

```sh
make setup generate build      # fetch deps, compile BPF + bindings, build bin/agent
make test                      # portable unit tests (run these anywhere, incl. macOS)
```

Full command reference: [`docs/HELPERCOMMANDS.md`](docs/HELPERCOMMANDS.md).

## Before opening a PR

1. `make test` passes (portable packages; works on any OS).
2. `go vet` is clean on the portable packages (`internal/metrics`, `internal/report`, `internal/server`, `internal/controller`).
3. On a Linux host, the eBPF packages build (`make build`) and — for detector changes — the acceptance test passes: `make stress`.
4. Keep the ✅ built / 🔜 planned tags in the docs accurate as features land.
5. Update [`docs/PROGRESS.md`](docs/PROGRESS.md) and [`CHANGELOG.md`](CHANGELOG.md) for user-visible changes.

## Commit & PR style

- Small, focused commits with a clear imperative subject.
- Reference the design section a change implements where relevant (e.g. "design §7.5").
- CI runs `go vet` + `go test -race` on the portable packages (see [`.github/workflows/ci.yml`](.github/workflows/ci.yml)). The eBPF packages are excluded there because the runner has no clang/BTF.

## License

By contributing you agree your contributions are licensed under the project's [Apache-2.0 license](LICENSE).
