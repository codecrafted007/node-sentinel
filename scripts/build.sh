#!/usr/bin/env bash
#
# build.sh — build node-sentinel binaries.
#
# Compiles the eBPF C, generates Go bindings (bpf2go), and builds every command
# under cmd/ into ./bin. Must run on a Linux host with kernel >= 5.10 (BTF +
# cgroups v2) and the toolchain: Go >= 1.25, clang/LLVM, bpftool, libbpf headers.
#
# Usage:
#   ./scripts/build.sh                  # BTF dump + generate bindings + build all cmds
#   ./scripts/build.sh --setup          # also fetch Go deps first (go get + tidy)
#   ./scripts/build.sh --tidy           # run `go mod tidy` before building
#   ./scripts/build.sh --skip-generate  # reuse existing bindings (skip BTF dump + bpf2go)
#   ./scripts/build.sh -h | --help
#
# Env overrides: GO, CLANG, BPFTOOL, OUTDIR
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Use the local Go toolchain only — fail loudly rather than silently downloading
# a newer one. Deps are pinned for Go 1.25 (see go.mod).
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

GO="${GO:-go}"
CLANG="${CLANG:-clang}"
BPFTOOL="${BPFTOOL:-bpftool}"
OUTDIR="${OUTDIR:-bin}"
VMLINUX="internal/ebpf/bpf/vmlinux.h"

DO_SETUP=0
DO_TIDY=0
DO_GENERATE=1

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
err()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; }
usage() { sed -n '3,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

for arg in "$@"; do
  case "$arg" in
    --setup)         DO_SETUP=1 ;;
    --tidy)          DO_TIDY=1 ;;
    --skip-generate) DO_GENERATE=0 ;;
    -h|--help)       usage; exit 0 ;;
    *)               err "unknown option: $arg"; usage; exit 2 ;;
  esac
done

# --- preflight ----------------------------------------------------------------
if [ "$(uname -s)" != "Linux" ]; then
  err "eBPF binaries build on Linux only (this host is $(uname -s)). Run on the Linux build host; 'go test ./internal/metrics/...' works anywhere."
  exit 1
fi

# Go is often under /usr/local/go/bin, which a non-login shell may miss.
if ! command -v "$GO" >/dev/null 2>&1 && [ -x /usr/local/go/bin/go ]; then
  GO=/usr/local/go/bin/go
fi

missing=0
need() { command -v "$1" >/dev/null 2>&1 || { err "missing required tool: $1"; missing=1; }; }
need "$GO"
need "$CLANG"
[ "$DO_GENERATE" = 1 ] && need "$BPFTOOL"
[ "$missing" = 1 ] && { err "install the toolchain (see README.md) and retry"; exit 1; }

if [ ! -f /sys/kernel/btf/vmlinux ]; then
  err "/sys/kernel/btf/vmlinux not found — kernel lacks BTF, CO-RE build not possible (need kernel >= 5.10)"
  exit 1
fi

log "go:      $($GO version)"
log "clang:   $($CLANG --version | head -1)"

# --- dependencies -------------------------------------------------------------
if [ "$DO_SETUP" = 1 ]; then
  log "fetching Go dependencies"
  "$GO" get github.com/cilium/ebpf@latest
  DO_TIDY=1
fi
if [ "$DO_TIDY" = 1 ]; then
  log "go mod tidy"
  "$GO" mod tidy
fi

# --- generate eBPF bindings ---------------------------------------------------
if [ "$DO_GENERATE" = 1 ]; then
  log "dumping kernel BTF -> $VMLINUX"
  "$BPFTOOL" btf dump file /sys/kernel/btf/vmlinux format c > "$VMLINUX"
  log "compiling BPF C + generating Go bindings (bpf2go)"
  "$GO" generate ./internal/ebpf/...
else
  log "skipping BPF generation (--skip-generate); reusing existing bindings"
fi

# --- build every command ------------------------------------------------------
mkdir -p "$OUTDIR"
built=0
for d in cmd/*/; do
  [ -f "${d}main.go" ] || continue
  name="$(basename "$d")"
  log "building $name -> $OUTDIR/$name"
  CGO_ENABLED=0 "$GO" build -o "$OUTDIR/$name" "./$d"
  built=$((built + 1))
done

[ "$built" -gt 0 ] || { err "no commands found under cmd/"; exit 1; }
log "done — built $built binary(ies):"
ls -la "$OUTDIR"
