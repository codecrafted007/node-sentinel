# node-sentinel — build (Phase 1: scheduler observer + standalone agent)
#
# eBPF builds on Linux only. On the build host:
#   make setup            # one-time: fetch Go deps
#   make vmlinux generate # dump BTF, compile BPF C + generate Go bindings
#   make build            # build bin/agent
#   sudo ./bin/agent
#
# `make test` runs the portable unit tests on any OS (including macOS).

GO      ?= go
BPFTOOL ?= bpftool
BIN     ?= bin
VMLINUX := internal/ebpf/bpf/vmlinux.h

.PHONY: all setup vmlinux generate build agent test clean docker-binaries docker-image stress overhead

all: build

## docker-binaries: cross-arch static binaries via Docker (any OS) -> bin/<os>_<arch>/
docker-binaries:
	./scripts/docker-build.sh binaries

## docker-image: build node-sentinel:dev via Docker (any OS), load into local daemon
docker-image:
	./scripts/docker-build.sh image

## setup: install Go deps (cilium/ebpf) — run once on the build host
setup:
	$(GO) get github.com/cilium/ebpf@latest
	$(GO) mod tidy

## vmlinux: dump the running kernel's BTF to a CO-RE header (Linux host w/ BTF)
vmlinux:
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > $(VMLINUX)

## generate: compile BPF C + generate Go bindings (needs clang + vmlinux.h)
generate:
	$(GO) generate ./internal/ebpf/...

## build: build the agent binary (Linux)
build:
	CGO_ENABLED=0 $(GO) build -o $(BIN)/agent ./cmd/agent

## agent: build and run (needs root or BPF+PERFMON+SYS_RESOURCE caps)
agent: build
	sudo ./$(BIN)/agent

## test: run portable unit tests (any OS)
test:
	$(GO) test ./internal/metrics/...

## stress: acceptance test — assert quiet-when-healthy, loud-under-contention (Linux, root)
stress: build
	sudo ./scripts/stress-test.sh

## overhead: measure agent CPU/RSS against the design §16 budget (Linux, root)
overhead: build
	sudo ./scripts/overhead.sh

## clean: remove build artifacts + generated bindings
clean:
	rm -rf $(BIN)
	rm -f internal/ebpf/sched_bpf*.go internal/ebpf/sched_bpf*.o
