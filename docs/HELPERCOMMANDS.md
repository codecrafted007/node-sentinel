# Helper commands — what's available & how to use it

A practical, copy-pasteable reference for every command in this repo: build, run, deploy, inspect, and validate. For *what the system does*, see [`CONCEPTS.md`](CONCEPTS.md) / [`ARCHITECTURE.md`](ARCHITECTURE.md); for *deploying it properly*, see [`DEPLOY.md`](DEPLOY.md).

> Placeholders like `<node-ip>`, `<jump-host>`, `<key>`, `<registry>` are yours to fill in.

---

## 1. Build

### The easy path — Docker (mac / win / linux, no toolchain)

The whole eBPF + Go build runs inside Docker. See [`docker-build.sh`](../scripts/docker-build.sh).

| Command | Result |
|---|---|
| `./scripts/docker-build.sh binaries` | cross-arch static binaries → `bin/linux_amd64/{agent,controller,sentinelctl}` + `bin/linux_arm64/...` |
| `./scripts/docker-build.sh image` | `node-sentinel:dev` for the **host arch**, loaded into the local docker |
| `./scripts/docker-build.sh image --push -t <registry>/node-sentinel:<tag>` | multi-arch (amd64+arm64) manifest pushed to a registry |
| `./scripts/docker-build.sh vmlinux` | regenerate the committed CO-RE header `internal/ebpf/bpf/vmlinux.h` |

Build a single explicit arch (e.g. amd64 image from an arm64 mac) directly:

```sh
docker buildx build --builder node-sentinel-builder \
  --platform linux/amd64 --target final -t node-sentinel:dev \
  -o type=docker,dest=node-sentinel-amd64.tar .
```

`--target artifact` exports just the binaries; `--target final` builds the runnable image.

### Native path — on a Linux host with the toolchain

```sh
make setup       # one-time: go get cilium/ebpf + go mod tidy
make generate    # compile BPF C + bpf2go bindings (needs clang; vmlinux.h is committed)
make build       # -> bin/agent
make test        # portable unit tests (work on any OS, incl. macOS)
./scripts/build.sh        # all three binaries -> bin/{agent,controller,sentinelctl}
#   ./scripts/build.sh --setup        also fetch Go deps first
#   ./scripts/build.sh --skip-generate  reuse existing bindings (skip BTF dump + bpf2go)
make vmlinux     # re-dump this kernel's BTF -> vmlinux.h (only when adding probes that read new structs)
make clean       # remove bin/ + generated bpf2go files
```

> `go build ./...` / `go test ./...` from macOS fails on the `//go:build linux` packages — expected. Build those on a Linux host or via Docker.

---

## 2. Run the agent (per node, needs root or BPF caps)

```sh
sudo ./bin/agent                                   # all defaults, standalone
sudo ./bin/agent --interval 5s --top 12            # read every 5s, show top 12
sudo ./bin/agent --controller-addr http://<controller-host>:8080   # report to a controller
```

**All agent flags** (defaults shown):

| Flag | Default | What it does |
|---|---|---|
| `--interval` | `5s` | how often maps are read & judged |
| `--top` | `20` | number of cgroups to display |
| `--cri-socket` | `unix:///run/containerd/containerd.sock` | CRI endpoint for pod resolution (CRI-O: point at its socket) |
| `--cgroup-root` | `/sys/fs/cgroup/kubepods.slice` | cgroups-v2 subtree scanned for pods |
| `--min-samples` | `100` | min run-queue samples before a pod counts as a CPU victim |
| `--runq-warn` | `5ms` | run-queue p99 a pod must exceed to flag CPU contention |
| `--deviation` | `3.0` | ×over a pod's *own* baseline p99 to count as a victim (once warm) |
| `--confidence` | `0.7` | offender confidence needed to **name** a pod the noisy neighbour |
| `--io-warn` | `20ms` | disk-I/O p99 latency a pod must exceed to flag an I/O victim |
| `--min-ops` | `20` | min completed I/O requests before a cgroup's I/O p99 is trusted |
| `--retrans-warn` | `10` | TCP retransmits/interval to flag a network victim |
| `--min-segs` | `50` | min `sendmsg` calls before retransmits are judged |
| `--metrics-addr` | `:2112` | Prometheus `/metrics` listen addr (empty string disables) |
| `--local-socket` | `/var/run/sentinel/agent.sock` | unix socket for `sentinelctl` (empty disables) |
| `--controller-addr` | _(empty)_ | controller URL; empty = fully standalone |
| `--node-name` | _(empty)_ | node name reported to the controller (set from `spec.nodeName` in k8s) |

Lower the `*-warn` thresholds on latency-sensitive nodes; defaults are deliberately conservative.

---

## 3. Run the controller (once per cluster)

```sh
sudo ./bin/controller                              # listen :8080, summary every 10s
./bin/controller --listen :19090 --log-interval 5s --stale-after 30s
```

| Flag | Default | What it does |
|---|---|---|
| `--listen` | `:8080` | address agents `POST /report` to |
| `--log-interval` | `10s` | how often the cluster summary is printed |
| `--stale-after` | `30s` | mark a node stale (DataGap) if no report arrives within this |

HTTP endpoints: `POST /report` (agents), `GET /status` (cluster JSON), `GET /healthz`.

---

## 4. Inspect live — sentinelctl

Reads the agent's local socket; run it on the node (or `kubectl exec` into the agent pod).

```sh
sudo ./bin/sentinelctl top              # htop-style live view (refreshes)
sudo ./bin/sentinelctl status           # one-shot snapshot
sudo ./bin/sentinelctl top --interval 2s --socket /var/run/sentinel/agent.sock
```

| Subcommand | Meaning |
|---|---|
| `top` (or no arg) | continuously refreshing view |
| `status` | print one snapshot and exit |

Flags: `--socket` (default `/var/run/sentinel/agent.sock`), `--interval` (default `2s`, for `top`).

---

## 5. Kubernetes — deploy & verify

```sh
# deploy (full cluster)
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/controller.yaml
kubectl apply -f deploy/agent.yaml

# OR pin the agent to ONE node first (safer first run on shared infra):
#   edit deploy/agent-singlenode.yaml: set kubernetes.io/hostname, then
kubectl apply -f deploy/namespace.yaml -f deploy/rbac.yaml -f deploy/agent-singlenode.yaml

# verify
kubectl -n sentinel-system get pods -o wide
kubectl -n sentinel-system logs ds/node-sentinel-agent | head        # "observers attached…"
kubectl -n sentinel-system logs deploy/node-sentinel-controller      # "[cluster] nodes=N healthy=H contended=C"

# live views
kubectl -n sentinel-system exec -it ds/node-sentinel-agent -- sentinelctl top
kubectl -n sentinel-system port-forward svc/node-sentinel-controller 8080:8080 &
curl -s localhost:8080/status | jq .

# uninstall
kubectl delete -f deploy/agent.yaml -f deploy/controller.yaml -f deploy/rbac.yaml -f deploy/namespace.yaml
```

If a pod is rejected by PodSecurity/admission: the agent needs `hostPID` + caps `BPF,PERFMON,SYS_RESOURCE,SYS_PTRACE`. Label the namespace `pod-security.kubernetes.io/enforce=privileged`, or set `privileged: true` in `deploy/agent.yaml`.

---

## 6. Get the image onto a node **without a registry**

When you can't (or don't want to) push to a registry, import the tar straight into a node's containerd. Build an arch-matched image (§1), then:

**Single-node / direct SSH:**
```sh
scp -i <key> node-sentinel-amd64.tar <user>@<node-ip>:~/
ssh -i <key> <user>@<node-ip> 'sudo ctr -n k8s.io images import ~/node-sentinel-amd64.tar'
```
The image imports as `docker.io/library/node-sentinel:dev`; a bare `node-sentinel:dev` + `imagePullPolicy: IfNotPresent` in the manifest matches it without pulling.

**Single-node / no SSH (via kubectl only)** — land a helper pod on the node, copy the tar in, import through the host's containerd:
```sh
# 1. helper pod pinned to the node, host root mounted at /host (privileged)
# 2. kubectl -n <ns> cp node-sentinel-amd64.tar <pod>:/host/tmp/img.tar
# 3. kubectl -n <ns> exec <pod> -- chroot /host /usr/bin/ctr -n k8s.io images import /tmp/img.tar
# 4. kubectl delete the helper pod
```

> Imported images live in the node's containerd store **until the node is recreated** — fine for tests, but a recycled (e.g. preemptible/spot) node loses it. For anything durable, push to a registry instead.

---

## 7. Validate detection & overhead (bare-metal / VM)

```sh
sudo ./scripts/stress-test.sh                                   # run a CPU hog, watch the agent catch it
sudo ./scripts/stress-test.sh --workers 8 --duration 60 --interval 5s --top 10
sudo ./scripts/overhead.sh                                      # measure agent CPU overhead (<1% target)
sudo ./scripts/overhead.sh --window 60 --stress-workers 4
```

`stress-test.sh` needs `stress-ng`; `overhead.sh` runs the agent under `systemd-run` and samples its CPU. Both are Linux-host tools.

**On Kubernetes**, force a confident detection with an idle-then-burst pod and watch the agent flip to `confident pod offender` — full walkthrough with real output in [`DETECTION-DEMO.md`](DETECTION-DEMO.md).

---

## 8. Observability quick hits

```sh
curl -s localhost:2112/metrics | grep sentinel_         # agent Prometheus metrics
curl -s localhost:2112/metrics | grep sentinel_node_contended   # the headline alert gauge
curl -s http://<controller-host>:8080/status | jq .     # cluster-wide JSON view
```

Pods are annotated `prometheus.io/scrape: "true"`, `prometheus.io/port: "2112"`. Alert on `sentinel_node_contended`.

---

## 9. Make targets (cheat sheet)

| Target | Does |
|---|---|
| `make setup` | fetch Go deps (run once on the build host) |
| `make vmlinux` | dump kernel BTF → `vmlinux.h` (only when adding probes) |
| `make generate` | compile BPF C + bpf2go bindings (needs clang) |
| `make build` | build `bin/agent` |
| `make agent` | build + `sudo` run the agent |
| `make test` | portable unit tests (any OS) |
| `make docker-binaries` | `./scripts/docker-build.sh binaries` |
| `make docker-image` | `./scripts/docker-build.sh image` |
| `make clean` | remove `bin/` + generated bindings |
