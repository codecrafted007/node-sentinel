#!/usr/bin/env bash
#
# docker-build.sh — build node-sentinel with Docker, on any OS (mac/win/linux).
#
# No local toolchain needed — clang, libbpf, Go, and the BPF compile all live in
# the build image (see Dockerfile). The BPF object is compiled once and the Go
# binaries are cross-compiled per arch, so this is fast and needs no emulation.
#
# Usage:
#   ./docker-build.sh binaries                 # cross-arch binaries -> ./bin/<os>_<arch>/
#   ./docker-build.sh image                    # image for the HOST arch, loaded into docker
#   ./docker-build.sh image --push -t R/N:T     # multi-arch manifest pushed to a registry
#   ./docker-build.sh vmlinux                   # regenerate the committed CO-RE header
#
# Env overrides:
#   PLATFORMS  target platforms (default: linux/amd64,linux/arm64)
#   IMAGE      image tag for `image`           (default: node-sentinel:dev)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
IMAGE="${IMAGE:-node-sentinel:dev}"

command -v docker >/dev/null 2>&1 || { echo "error: docker not found" >&2; exit 1; }

# Multi-platform builds need the docker-container driver (the default "docker"
# driver can't do them). Provision a dedicated builder once and reuse it.
BUILDER="node-sentinel-builder"
ensure_builder() {
  if ! docker buildx inspect "$BUILDER" >/dev/null 2>&1; then
    echo "==> creating buildx builder '$BUILDER' (docker-container driver)"
    docker buildx create --name "$BUILDER" --driver docker-container >/dev/null
  fi
}

cmd="${1:-binaries}"; shift || true

case "$cmd" in
  binaries)
    # Build every target platform and export just the binaries to ./bin.
    # buildx writes one subdir per platform: bin/linux_amd64, bin/linux_arm64.
    ensure_builder
    docker buildx build --builder "$BUILDER" --platform "$PLATFORMS" \
      --target artifact --output "type=local,dest=bin" "$@" .
    echo
    echo "binaries:"
    find bin -type f \( -name agent -o -name controller -o -name sentinelctl \) | sort
    ;;

  image)
    # A multi-arch manifest list can't be loaded into the local docker image
    # store — it must be pushed. So: with --push, build all PLATFORMS and push;
    # without, build the host arch only and --load it for immediate local use.
    if [[ " $* " == *" --push "* ]]; then
      ensure_builder
      docker buildx build --builder "$BUILDER" --platform "$PLATFORMS" \
        --target final -t "$IMAGE" "$@" .
    else
      # Host arch only, loaded into the local daemon for immediate use. The
      # default docker driver handles --load fine, so no special builder needed.
      docker buildx build --target final -t "$IMAGE" --load "$@" .
      echo "loaded $IMAGE (host arch). For a multi-arch push: $0 image --push -t <registry>/<name>:<tag>"
    fi
    ;;

  vmlinux)
    # Regenerate internal/ebpf/bpf/vmlinux.h from a Linux kernel's BTF, via a
    # throwaway container (works on macOS/Windows: uses the Docker VM kernel's
    # BTF). Only needed when probes start reading new kernel structs.
    docker run --rm -v "$ROOT/internal/ebpf/bpf:/out" debian:bookworm bash -c '
      apt-get update -qq >/dev/null && apt-get install -y -qq bpftool >/dev/null &&
      bpftool btf dump file /sys/kernel/btf/vmlinux format c > /out/vmlinux.h &&
      echo "wrote /out/vmlinux.h ($(wc -l < /out/vmlinux.h) lines)"'
    ;;

  -h|--help|"")
    sed -n '3,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    ;;

  *)
    echo "error: unknown command: $cmd (try: binaries | image | vmlinux)" >&2
    exit 2
    ;;
esac
