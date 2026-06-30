// Remediation is the controller's "act" half (issue #7) — the first time
// node-sentinel does anything but observe. It stays deliberately conservative:
//
//   - It acts ONLY on offenders the per-node confidence model already marked
//     confident (Confidence >= the snapshot's ConfidenceMin). Low-confidence
//     findings remain alert-only.
//   - Off by default. The controller is observe-only unless remediation is
//     explicitly enabled, and a dry-run mode logs intended actions without
//     touching the API.
//   - Tiered and visibility-first. The primary tier (when --resize is set) is an
//     in-place /resize (KEP-1287): the kubelet itself lowers the offender's CPU
//     limit to its request, then it is restored after a fixed window (resize.go).
//     The mandatory fallback is a Kubernetes Event (Reason: NoisyNeighborThrottled)
//     — so a throttled pod is never a silent mystery, and clusters without
//     in-place resize still get an alert. Every resize is itself announced by an Event.
//   - Per-pod cooldown so one sustained offender doesn't spam actions every
//     report interval.
//
// Portable Go (client-go, no eBPF) — builds and unit-tests anywhere.
package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/codecrafted007/node-sentinel/internal/report"
)

// EventReason is the Reason stamped on every remediation Event, so operators can
// alert/filter on it (`kubectl get events --field-selector reason=...`).
const EventReason = "NoisyNeighborThrottled"

// RemediationConfig tunes the remediator.
type RemediationConfig struct {
	// DryRun logs the intended action but makes no API call. The control loop is
	// fully exercised; nothing is mutated.
	DryRun bool
	// Cooldown is the minimum time between actions on the same pod, so a steady
	// offender produces one action per cooldown, not one per report.
	Cooldown time.Duration
	// Resize enables the in-place /resize tier (KEP-1287) for CPU offenders: lower
	// the limit to the request, then restore it after RestoreAfter. When false (or
	// the pod can't be resized), remediation is Event-only.
	Resize bool
	// RestoreAfter is how long a /resize throttle stays in place before it is
	// lifted back to the original limit ("timeout, not eviction").
	RestoreAfter time.Duration
	// Namespaces, when non-empty, restricts remediation to offenders in these
	// namespaces — the safe way to roll out actuation (start with one namespace).
	// Empty means act in every namespace.
	Namespaces []string
	// ConfidenceThreshold, when > 0, overrides the snapshot's gate — a
	// controller-side floor a NodeHealthPolicy can set. 0 = use the snapshot's
	// own ConfidenceMin.
	ConfidenceThreshold float64
}

// target is one pod the snapshot named as a confident offender.
type target struct {
	namespace, pod, container string
	resource                  string // "cpu" | "disk" | "net"
	confidence                float64
}

func (t target) key() string { return t.namespace + "/" + t.pod }

// Remediator turns confident-offender snapshots into Kubernetes actions.
type Remediator struct {
	client kubernetes.Interface
	cfg    RemediationConfig
	now    func() time.Time

	mu        sync.Mutex
	lastActed map[string]time.Time      // pod key -> last action time (cooldown)
	throttled map[string]*throttleState // pod key -> active /resize throttle awaiting restore

	nsAllow map[string]bool // nil/empty = all namespaces

	stateNS   string     // namespace for the durable throttle ConfigMap ("" = in-memory only)
	persistMu sync.Mutex // serializes read-modify-write of the throttle ConfigMap
}

// NewRemediator builds a remediator over a Kubernetes client.
func NewRemediator(client kubernetes.Interface, cfg RemediationConfig) *Remediator {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Minute
	}
	if cfg.RestoreAfter <= 0 {
		cfg.RestoreAfter = 10 * time.Minute
	}
	var nsAllow map[string]bool
	if len(cfg.Namespaces) > 0 {
		nsAllow = make(map[string]bool, len(cfg.Namespaces))
		for _, ns := range cfg.Namespaces {
			nsAllow[ns] = true
		}
	}
	return &Remediator{
		client:    client,
		cfg:       cfg,
		now:       time.Now,
		lastActed: map[string]time.Time{},
		throttled: map[string]*throttleState{},
		nsAllow:   nsAllow,
	}
}

// Remediate evaluates one node's snapshot and acts on every confident offender
// that is out of cooldown. It never returns an error: a single pod's failure
// must not stop the others, and remediation must never disrupt detection.
func (r *Remediator) Remediate(ctx context.Context, s report.Snapshot) {
	for _, t := range confidentTargets(s, r.cfg.ConfidenceThreshold) {
		if r.nsAllow != nil && !r.nsAllow[t.namespace] {
			continue // remediation not enabled for this namespace
		}
		if r.isThrottled(t.key()) {
			continue // already throttled and awaiting restore
		}
		if !r.takeCooldown(t.key()) {
			continue // acted on this pod recently
		}
		r.act(ctx, t, s)
	}
}

// act applies the tiered remediation for one confident offender: try the
// /resize tier first (CPU only, when enabled), and fall back to an Event.
func (r *Remediator) act(ctx context.Context, t target, s report.Snapshot) {
	why := fmt.Sprintf("confident %s noisy-neighbour (%.0f%% >= %.0f%% confidence) on node %s",
		t.resource, t.confidence*100, s.ConfidenceMin*100, s.NodeName)

	if r.cfg.DryRun {
		tier := "Event"
		if r.cfg.Resize && t.resource == "cpu" {
			tier = "resize→Event"
		}
		fmt.Printf("[remediate] DRY-RUN would %s: %s (%s)\n", tier, t.key(), why)
		return
	}

	// Primary tier: in-place /resize (CPU offenders only). On success it emits
	// its own Event; on any failure we fall through to the Event tier.
	if r.cfg.Resize && t.resource == "cpu" && r.throttle(ctx, t, why) {
		return
	}

	// Fallback tier: a Warning Event (always available).
	msg := "node-sentinel: " + t.key() + " is a " + why + " — throttling recommended"
	if err := r.emitEvent(ctx, t, msg); err != nil {
		fmt.Printf("[remediate] Event for %s failed: %v\n", t.key(), err)
		r.clearCooldown(t.key()) // let the next report retry
		return
	}
	fmt.Printf("[remediate] Event emitted: %s\n", msg)
}

// isThrottled reports whether a pod already has a /resize throttle in flight.
func (r *Remediator) isThrottled(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.throttled[key]
	return ok
}

// takeCooldown returns true and records the action time if the pod is out of
// cooldown; false if it acted within the cooldown window.
func (r *Remediator) takeCooldown(key string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastActed[key]; ok && now.Sub(last) < r.cfg.Cooldown {
		return false
	}
	r.lastActed[key] = now
	return true
}

func (r *Remediator) clearCooldown(key string) {
	r.mu.Lock()
	delete(r.lastActed, key)
	r.mu.Unlock()
}

// emitEvent attaches a Warning Event to the offending pod (the mandatory tier).
// It looks the pod up first to anchor the Event to the real object (UID), but
// still emits by name if the pod has already gone.
func (r *Remediator) emitEvent(ctx context.Context, t target, message string) error {
	ref := corev1.ObjectReference{Kind: "Pod", Namespace: t.namespace, Name: t.pod}
	if pod, err := r.client.CoreV1().Pods(t.namespace).Get(ctx, t.pod, metav1.GetOptions{}); err == nil {
		ref.UID = pod.UID
		ref.ResourceVersion = pod.ResourceVersion
	}

	now := metav1.NewTime(r.now())
	_, err := r.client.CoreV1().Events(t.namespace).Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "noisy-neighbor-",
			Namespace:    t.namespace,
		},
		InvolvedObject: ref,
		Reason:         EventReason,
		Message:        message,
		Type:           corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "node-sentinel-controller"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}, metav1.CreateOptions{})
	return err
}

// confidentTargets extracts the pods a snapshot named as confident offenders
// across all three dimensions: a real namespace/pod/container identity
// (system/unattributed cgroups are never remediated — the honest-attribution
// rule) at or above the confidence gate. gate, when > 0, overrides the
// snapshot's own ConfidenceMin (a NodeHealthPolicy can tighten the bar).
func confidentTargets(s report.Snapshot, gate float64) []target {
	if gate <= 0 {
		gate = s.ConfidenceMin
	}
	var out []target
	add := func(pod string, conf float64, resource string) {
		if conf < gate {
			return
		}
		if ns, p, c, ok := splitPod(pod); ok {
			out = append(out, target{ns, p, c, resource, conf})
		}
	}
	for _, o := range s.Offenders {
		add(o.Pod, o.Confidence, "cpu")
	}
	for _, o := range s.IOOffenders {
		add(o.Pod, o.Confidence, "disk")
	}
	for _, o := range s.NetOffenders {
		add(o.Pod, o.Confidence, "net")
	}
	return out
}

// splitPod parses an offender label "namespace/pod/container". It returns ok=false
// for anything that isn't a real container (system(cg:..), unknown), which must
// never be acted on.
func splitPod(label string) (ns, pod, container string, ok bool) {
	parts := strings.SplitN(label, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
