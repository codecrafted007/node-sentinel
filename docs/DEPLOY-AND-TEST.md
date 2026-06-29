# Deploy & test node-sentinel end-to-end

A single, copy-pasteable runbook: build the image → get it onto a cluster →
deploy (observe-only) → enable remediation (flags **or** the `NodeHealthPolicy`
CRD) → trigger a confident offender and watch it get throttled and restored →
tear down. Placeholders look like `<this>` — substitute your own values.

> Just want the manifests reference? See [`DEPLOY.md`](DEPLOY.md). Want the plain-
> English model of what it's doing? [`CONCEPTS.md`](CONCEPTS.md) and the
> [`DEEPDIVE.md`](DEEPDIVE.md).

---

## 0. Prerequisites

- **A Kubernetes cluster.** Agents run on Linux nodes only — kernel **≥ 5.10 with
  BTF**, **cgroups v2**. (The controller is portable.)
- **`kubectl`** with cluster access.
- **Docker** to build (no local Go/clang/eBPF toolchain needed — the build is
  hermetic). 
- **For the in-place `/resize` remediation tier:** the apiserver must expose the
  `pods/resize` subresource (feature `InPlacePodVerticalScaling`; beta / on by
  default in recent Kubernetes). Verify:
  ```sh
  kubectl get --raw /api/v1 | tr ',' '\n' | grep '"name":"pods/resize"'
  ```
  If it's absent, `enforce` mode automatically falls back to Events — nothing
  breaks. (Note: a `kubectl` older than v1.32 can't use `--subresource resize`
  from the CLI; the controller uses client-go and doesn't need it.)
- **For the registry-less image path (§2B):** SSH access to the nodes and
  containerd's `ctr` on them.

---

## 1. Build the image

The whole eBPF + Go build runs inside Docker (see [`HOW.md`](HOW.md)).

**With a registry (production):**
```sh
./scripts/docker-build.sh image --push -t <registry>/node-sentinel:<tag>   # multi-arch
```

**Registry-less (test / air-gapped)** — build a single-arch image to a tar that
matches your nodes' CPU arch (`amd64` or `arm64`):
```sh
docker buildx build --platform linux/<arch> --target final \
  -t node-sentinel:dev -o type=docker,dest=node-sentinel-<arch>.tar .
```

---

## 2. Get the image onto the cluster

### 2A. Via a registry (production)
Push (above), then set `image:` in `deploy/agent.yaml` and `deploy/controller.yaml`
to `<registry>/node-sentinel:<tag>`.

### 2B. Registry-less import (test)
Copy the tar to each node and import it into containerd's Kubernetes namespace:
```sh
for NODE in <node-ip-1> <node-ip-2> <node-ip-3>; do
  scp -i ~/.ssh/<node-key> node-sentinel-<arch>.tar <user>@"$NODE":~/
  ssh -i ~/.ssh/<node-key> <user>@"$NODE" \
    'sudo ctr -n k8s.io images import ~/node-sentinel-<arch>.tar'
done
```
- It imports as `docker.io/library/node-sentinel:dev`; the manifests use
  `node-sentinel:dev` with `imagePullPolicy: IfNotPresent`, so the kubelet uses
  the imported image and never pulls.
- If nodes sit behind a bastion/jump host, run the loop from there. Different node
  pools may need different SSH keys — substitute per node.
- **To update later:** re-import the new tar on each node, then
  `kubectl -n sentinel-system rollout restart ds/node-sentinel-agent deploy/node-sentinel-controller`.

---

## 3. Deploy — observe-only first (safe default)

```sh
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/agent.yaml        # DaemonSet: one agent per node
kubectl apply -f deploy/controller.yaml   # Deployment: cluster aggregator (no actions yet)
```

Verify detection is live:
```sh
kubectl -n sentinel-system get pods -o wide
kubectl -n sentinel-system logs ds/node-sentinel-agent | grep -E "observer attached|resolver:"
kubectl -n sentinel-system logs deploy/node-sentinel-controller   # [cluster] summary lines
```
Each interval the agent prints a healthy heartbeat; under contention it prints
per-dimension OFFENDER/VICTIM tables with confidence. The controller stays
**observe-only** until you opt in below.

---

## 4. Enable remediation

Two interchangeable ways. Both act **only** on confident offenders, both are off
until you turn them on, and both keep a per-pod cooldown.

### 4A. Flags (quick)
Add args to the controller container (`deploy/controller.yaml` `command:`, or
`kubectl -n sentinel-system edit deploy/node-sentinel-controller`):
```
--remediate                       # turn remediation on (Event tier)
--resize                          # also use the in-place /resize CPU throttle tier
--remediate-namespaces=<ns>       # scope it (recommended — start with ONE namespace)
--restore-after=10m
--cooldown=5m
--dry-run                         # optional: log intended actions, touch nothing
```

### 4B. NodeHealthPolicy CRD (recommended — declarative)
```sh
kubectl apply -f deploy/crd.yaml
kubectl wait --for=condition=Established crd/nodehealthpolicies.sentinel.io --timeout=30s
# edit mode / scope first, then:
kubectl apply -f deploy/nodehealthpolicy-sample.yaml
```
Run the controller with **`--policy`** (it reads the CR and ignores the
remediation flags). The policy's `mode` is the master switch:

| `mode` | behaviour |
|---|---|
| `observe` | aggregate only — no Events, no actions |
| `alert` | emit a `NoisyNeighborThrottled` Warning Event on confident offenders |
| `enforce` | in-place `/resize` CPU throttle (auto-restored), falling back to Events |

A minimal enforcing, single-namespace policy:
```yaml
apiVersion: sentinel.io/v1alpha1
kind: NodeHealthPolicy
metadata: { name: default }
spec:
  mode: enforce
  priority: 100
  attribution: { confidenceThreshold: 0.7 }
  remediation: { resize: true, cooldown: 5m, restoreAfter: 2m, namespaces: ["<ns>"] }
```
Confirm the controller picked it up:
```sh
kubectl -n sentinel-system logs deploy/node-sentinel-controller | grep policy
# → policy "default" (priority 100): mode=enforce → remediation ACTIVE
#   (resize=true, cooldown=5m0s, restore-after=2m0s, namespaces=[<ns>])
```

---

## 5. Test it — trigger a confident offender

A pod scores as a **confident CPU offender** only when it: **(a)** bursts above
its *own* learned baseline, **(b)** holds a meaningful share of CPU, and **(c)**
actually starves neighbours. The recipe: **idle first** (warms a low baseline),
**then burst**; set `requests` ≪ `limits` (so it reads as "over fair share"); and
add a `resizePolicy` so the `/resize` tier can throttle it in place.

```yaml
apiVersion: v1
kind: Pod
metadata: { name: noisy-offender, namespace: <ns> }   # <ns> = a remediated namespace
spec:
  # Pin to a node that has the image and a running agent.
  nodeName: <node-name>
  restartPolicy: Never
  tolerations: [{ operator: Exists }]
  containers:
    - name: hog
      image: busybox:1.36
      # idle 45s to warm a LOW baseline, then peg the CPU
      command: ["sh","-c","sleep 45; for i in $(seq 1 8); do while true; do :; done & done; sleep 200"]
      resources:
        requests: { cpu: 100m, memory: 16Mi }
        limits:   { cpu: "3",  memory: 64Mi }   # request ≪ limit, and a limit to throttle
      resizePolicy:
        - resourceName: cpu
          restartPolicy: NotRequired             # resize CPU without a restart
```
```sh
kubectl apply -f noisy-offender.yaml
```

Watch it get caught, throttled, and restored (≈ idle + a few intervals):
```sh
# controller decisions
kubectl -n sentinel-system logs deploy/node-sentinel-controller | grep -E "throttled|restored"

# the pod's CPU limit, live: 3 → 100m during the throttle, back to 3 after restoreAfter
kubectl -n <ns> get pod noisy-offender \
  -o jsonpath='{.spec.containers[0].resources.limits.cpu}{"\n"}'

# the audit trail
kubectl get events -A --field-selector reason=NoisyNeighborThrottled
```
Expected sequence:
```
[remediate] throttled <ns>/noisy-offender CPU limit 3→100m (confident cpu noisy-neighbour
            100% >= 70% confidence) on node <node-name>; restoring at <time>
            ... limit live = 100m ...
[remediate] restored <ns>/noisy-offender CPU limit to 3 (throttle window elapsed)
            ... limit live = 3 ...
```
For **`alert`** mode you'll see the Event but no limit change; for **`observe`**,
neither — the offender just shows in the agent/controller logs.

Always delete the test pod afterwards: `kubectl -n <ns> delete pod noisy-offender`.

---

## 6. Teardown

```sh
kubectl delete -f deploy/nodehealthpolicy-sample.yaml --ignore-not-found   # if you used the CRD
kubectl delete -f deploy/crd.yaml --ignore-not-found
kubectl delete namespace sentinel-system                                   # agents, controller, SAs
kubectl delete clusterrole,clusterrolebinding node-sentinel-controller --ignore-not-found
```
Registry-less: also remove the imported image from each node:
```sh
for NODE in <node-ip-1> <node-ip-2> <node-ip-3>; do
  ssh -i ~/.ssh/<node-key> <user>@"$NODE" \
    'sudo ctr -n k8s.io images rm docker.io/library/node-sentinel:dev; rm -f ~/node-sentinel-*.tar'
done
```

---

## Safety & gotchas

- **Off by default, escalating.** `observe → alert → enforce`. Every action is
  confidence-gated, cooldown-limited, namespace-scopeable, and (for `/resize`)
  auto-restored. The controller's RBAC grants only `events` + `pods/resize` (and
  reads pods/policies) — **no evict, no delete**, no patch of the pod spec proper.
- **Scope before you enforce.** Roll out with `namespaces`/`--remediate-namespaces`
  set to one namespace you own; widen once you trust it.
- **`/resize` throttle state is in-memory.** If the controller restarts mid-window,
  a pending restore is lost and the pod stays throttled until its next
  restart/redeploy. (Durable restore state is a roadmap item — don't run unattended
  `enforce` in production yet.)
- **In-place resize availability.** If your cluster lacks `pods/resize`, `enforce`
  degrades to the Event tier automatically.
- **Detection thresholds are still agent flags** (`--runq-warn`, `--io-warn`,
  `--retrans-warn`/`--retrans-rate-warn`, `--min-samples`, `--deviation`,
  `--confidence`) — not yet driven by the policy. The CRD configures the
  controller's *remediation*, not the agents' *detection*, today.
