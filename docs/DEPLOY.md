# Deploying & running node-sentinel

This is the practical "how do I actually run it" guide. New to the project? Read [`CONCEPTS.md`](CONCEPTS.md) first.

## What you're deploying

| Component | Runs | As | Does |
|-----------|------|----|----|
| **agent** | on every node | DaemonSet | loads the eBPF observers, detects contention, resolves cgroups → pods, reports to the controller |
| **controller** | once per cluster | Deployment | aggregates per-node reports into a cluster-wide view |
| **sentinelctl** | on demand | `kubectl exec` into an agent pod | live `top`/`status` CLI |

The agent is **self-contained** — it works standalone (metrics + local CLI) even with no controller. The controller is currently **observe-only**: it shows the cluster picture; it does not yet take action.

Two ways to run it: **[Kubernetes](#a-kubernetes)** (the real way) or **[bare binaries / systemd](#b-bare-binaries--systemd)** (fastest to try, what the project is tested with).

## Prerequisites (every node)

- Linux kernel **≥ 5.10** with BTF (`/sys/kernel/btf/vmlinux` exists) and **cgroups v2** (`stat -fc %T /sys/fs/cgroup` → `cgroup2fs`)
- A CRI runtime — **containerd** at `/run/containerd/containerd.sock` (CRI-O works too; pass `--cri-socket`)
- To **build**: just **Docker** on any OS (`./scripts/docker-build.sh`, which carries the whole toolchain in-image) — or, for the native path, Go ≥ 1.25 + clang/LLVM + libbpf-dev + bpftool + make on a Linux host. See [`README.md`](../README.md#build-with-docker-any-os--the-easy-path).

---

## A. Kubernetes

### 1. Build the image

The whole build (BPF compile + static Go binaries) runs inside Docker, so you can do this on **any OS** — no clang/libbpf/Go needed locally:

```sh
./scripts/docker-build.sh image                                          # host arch, loaded into docker
# or, for a mixed-arch cluster, push a multi-arch manifest to a registry:
./scripts/docker-build.sh image --push -t <registry>/node-sentinel:<tag>
```

(Have the toolchain on a Linux host and prefer the native path? `./scripts/build.sh` still produces `bin/{agent,controller,sentinelctl}`. The Dockerfile builds from source either way.)

### 2. Make the image available to the cluster

**Single-node / no registry** — import the image straight into containerd's k8s namespace:

```sh
docker save node-sentinel:dev | sudo ctr -n k8s.io images import -
```

(The manifests use `imagePullPolicy: IfNotPresent`, so a locally-present image is used as-is.) **Multi-node** — push to a registry the nodes can pull from and set that image in `deploy/agent.yaml` + `deploy/controller.yaml`.

### 3. Apply the manifests

```sh
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/controller.yaml
kubectl apply -f deploy/agent.yaml
```

### 4. Verify

```sh
kubectl -n sentinel-system get pods -o wide        # agent on every node + 1 controller, all Running
kubectl -n sentinel-system logs ds/node-sentinel-agent | head      # "observers attached..."
kubectl -n sentinel-system logs deploy/node-sentinel-controller    # "[cluster] nodes=N healthy=H contended=C"
```

### 5. Use it

```sh
# live per-node view (htop-style)
kubectl -n sentinel-system exec -it ds/node-sentinel-agent -- sentinelctl top

# cluster view from the controller
kubectl -n sentinel-system port-forward svc/node-sentinel-controller 8080:8080 &
curl -s localhost:8080/status | jq .
```

Prometheus: each agent exposes `/metrics` on port `2112` (pods are annotated `prometheus.io/scrape`). Alert on `sentinel_node_contended`.

> **eBPF capabilities:** the agent runs with `BPF, PERFMON, SYS_RESOURCE, SYS_PTRACE` (not full privilege). If it fails to load BPF on your runtime, set `privileged: true` in `deploy/agent.yaml` and re-apply.

---

## B. Bare binaries / systemd

No Kubernetes needed — good for bare-metal/VM nodes, and this is exactly how the project is developed and tested.

### Controller (run once, on any reachable host)

```sh
./scripts/build.sh
sudo ./bin/controller --listen :19090 --log-interval 5s
# POST /report, GET /status, GET /healthz
```

### Agent (run on each node, as root)

```sh
sudo ./bin/agent \
  --interval 5s \
  --controller-addr http://<controller-host>:19090
# omit --controller-addr to run fully standalone
```

As a systemd unit:

```sh
sudo systemd-run --unit=node-sentinel-agent --collect \
  /path/to/bin/agent --interval 5s --controller-addr http://<controller-host>:19090
sudo journalctl -u node-sentinel-agent -f
```

### Use it

```sh
sudo ./bin/sentinelctl top         # live view (reads /var/run/sentinel/agent.sock)
sudo ./bin/sentinelctl status      # one-shot
curl -s http://<controller-host>:19090/status   # cluster view
curl -s localhost:2112/metrics | grep sentinel_ # agent metrics
```

Validate detection end-to-end with [`./scripts/stress-test.sh`](../README.md#stress-testing--validation) and overhead with `./scripts/overhead.sh`.

---

## Tuning

Detection thresholds are agent flags (see [`README.md`](../README.md#run-on-a-linux-host-kernel--510-with-btf--cgroups-v2)): `--runq-warn`, `--io-warn`, `--retrans-warn`, `--min-samples`, `--deviation`, `--confidence`. Defaults are conservative; latency-sensitive nodes warrant lower thresholds. (A `NodeHealthPolicy` CRD will make these cluster-policy-driven in a later slice.)

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| agent CrashLoop, "verifier"/BTF error | kernel < 5.10 or no BTF; check `ls /sys/kernel/btf/vmlinux` |
| agent can't load BPF in-pod | set `privileged: true` in `deploy/agent.yaml` |
| all pods show `system(cg:N)`, not names | CRI socket wrong/unmounted — check the `cri` volume path matches your runtime |
| controller shows `stale=N` | agents can't reach it — check `--controller-addr` / the Service DNS |
| `address already in use` | the chosen port is taken (e.g. 8080 is common) — pick another |

## Uninstall

```sh
kubectl delete -f deploy/agent.yaml -f deploy/controller.yaml -f deploy/rbac.yaml -f deploy/namespace.yaml
# binaries: sudo systemctl stop node-sentinel-agent
```

---

**Status note:** the bare-binary path (B) is what node-sentinel is continuously tested with — agent + controller validated end-to-end on a live single-node cluster. For the Kubernetes path (A), the **image builds and imports into containerd cleanly**, and the manifests follow the design's deployment topology (§6.8). The `kubectl apply` step itself could not be verified on the test cluster because that cluster's **API discovery is degraded** (`couldn't get current server API group list` — a pre-existing cluster issue, typically a down aggregated APIService such as metrics-server, unrelated to these manifests). On a healthy cluster the apply flow is standard. Treat A as "ready to try," B as "known-good."
