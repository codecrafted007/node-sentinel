# node-sentinel — self-contained multi-arch build.
#
# Builds the eBPF bytecode and the static Go binaries entirely inside Docker, so
# a dev on macOS / Windows / Linux needs only Docker — no clang, libbpf, bpftool,
# or Go toolchain installed locally. Use ./docker-build.sh as the front door:
#
#   ./docker-build.sh binaries     # -> bin/linux_amd64/{agent,controller,sentinelctl} + linux_arm64/...
#   ./docker-build.sh image        # -> node-sentinel:dev loaded into the local daemon (host arch)
#   ./docker-build.sh image --push -t <registry>/node-sentinel:<tag>   # multi-arch manifest
#
# Why this is fast and arch-clean:
#   - The BPF object is compiled ONCE. Our probes use tp_btf/fentry only (no
#     pt_regs / syscall regs), so the bytecode is CPU-arch-independent; it's
#     little-endian (bpfel), and amd64 + arm64 are both LE. CO-RE relocates the
#     struct field offsets against the running kernel at load time.
#   - The Go binaries are CGO_ENABLED=0, so they cross-compile per GOARCH with no
#     C toolchain and no QEMU emulation — the builder stays on the build platform.
#   - vmlinux.h is committed (internal/ebpf/bpf/vmlinux.h), so the build is
#     hermetic: no kernel BTF needed on the dev's machine.

# syntax=docker/dockerfile:1

# ---- builder: compile BPF once, then cross-compile Go per target arch --------
# Pinned to the BUILD platform (native) so the Go cross-compile needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS builder

# eBPF toolchain only: clang compiles the BPF C, libbpf-dev supplies <bpf/*.h>.
# bpftool is intentionally absent — vmlinux.h is committed, so we never dump BTF.
RUN apt-get update \
 && apt-get install -y --no-install-recommends clang llvm libbpf-dev \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /src
ENV GOTOOLCHAIN=local

# Module cache layer — only re-runs when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Compile the BPF C + generate the Go bindings ONCE (arch-independent, see above).
RUN go generate ./internal/ebpf/...

# Cross-compile the three static binaries for the requested target arch.
ARG TARGETOS TARGETARCH
RUN set -eux; \
    for cmd in agent controller sentinelctl; do \
      CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
        go build -trimpath -ldflags="-s -w" -o "/out/${cmd}" "./cmd/${cmd}"; \
    done

# ---- artifact: binaries only, for `buildx --output type=local` ---------------
# A scratch stage so exporting to the host yields just the three binaries
# (per platform: bin/linux_amd64/agent, bin/linux_arm64/agent, ...).
FROM scratch AS artifact
COPY --from=builder /out/agent       /agent
COPY --from=builder /out/controller  /controller
COPY --from=builder /out/sentinelctl /sentinelctl

# ---- final: the deployable image (distroless, static) ------------------------
FROM gcr.io/distroless/static-debian12 AS final
COPY --from=builder /out/agent       /usr/local/bin/agent
COPY --from=builder /out/controller  /usr/local/bin/controller
COPY --from=builder /out/sentinelctl /usr/local/bin/sentinelctl
# Agent is the default; the controller Deployment overrides command, sentinelctl
# is run via `kubectl exec` into an agent pod.
ENTRYPOINT ["/usr/local/bin/agent"]
