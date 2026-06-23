// Remediation is the controller's "act" half (issue #7) — the first time
// node-sentinel does anything but observe. It stays deliberately conservative:
//
//   - It acts ONLY on offenders the per-node confidence model already marked
//     confident (Confidence >= the snapshot's ConfidenceMin). Low-confidence
//     findings remain alert-only.
//   - Off by default. The controller is observe-only unless remediation is
//     explicitly enabled, and a dry-run mode logs intended actions without
//     touching the API.
//   - Tiered and visibility-first. The mandatory tier is a Kubernetes Event
//     (Reason: NoisyNeighborThrottled) so a throttled pod is never a silent
//     mystery. The in-place /resize tier (KEP-1287) layers on top later.
//   - Per-pod cooldown so one sustained offender doesn't spam Events every
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
	// offender produces one Event per cooldown, not one per report.
	Cooldown time.Duration
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

	mu       sync.Mutex
	lastActed map[string]time.Time // pod key -> last action time (cooldown)
}

// NewRemediator builds a remediator over a Kubernetes client.
func NewRemediator(client kubernetes.Interface, cfg RemediationConfig) *Remediator {
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Minute
	}
	return &Remediator{
		client:    client,
		cfg:       cfg,
		now:       time.Now,
		lastActed: map[string]time.Time{},
	}
}

// Remediate evaluates one node's snapshot and acts on every confident offender
// that is out of cooldown. It never returns an error: a single pod's failure
// must not stop the others, and remediation must never disrupt detection.
func (r *Remediator) Remediate(ctx context.Context, s report.Snapshot) {
	for _, t := range confidentTargets(s) {
		if !r.takeCooldown(t.key()) {
			continue // acted on this pod recently
		}
		msg := fmt.Sprintf("node-sentinel: %s is a confident %s noisy-neighbour (%.0f%% >= %.0f%% confidence) on node %s — throttling recommended",
			t.key(), t.resource, t.confidence*100, s.ConfidenceMin*100, s.NodeName)

		if r.cfg.DryRun {
			fmt.Printf("[remediate] DRY-RUN would Event: %s\n", msg)
			continue
		}
		if err := r.emitEvent(ctx, t, msg); err != nil {
			fmt.Printf("[remediate] Event for %s failed: %v\n", t.key(), err)
			r.clearCooldown(t.key()) // let the next report retry
			continue
		}
		fmt.Printf("[remediate] Event emitted: %s\n", msg)
	}
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
// across all three dimensions: Confidence at or above the snapshot's gate, and a
// real namespace/pod/container identity (system/unattributed cgroups are never
// remediated — the honest-attribution rule).
func confidentTargets(s report.Snapshot) []target {
	gate := s.ConfidenceMin
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
