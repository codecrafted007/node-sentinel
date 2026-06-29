// The /resize tier (issue #7, KEP-1287). Instead of a blunt evict — or fighting
// the kubelet by writing cgroups directly — we patch the offending pod's
// /resize subresource so the *kubelet itself* lowers its CPU limit to the
// request. It's "timeout, not eviction": the throttle is recorded and lifted
// after a fixed window (RestoreAfter). Every throttle and restore is announced
// by an Event, so the change is never silent.
//
// Falls back to the Event tier (remediation.go) when the pod has no usable CPU
// limit to lower, or the apiserver/kubelet rejects the resize.
package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// restoreTick is how often the restore loop checks for throttles whose window
// has elapsed.
const restoreTick = 30 * time.Second

// throttleState records a /resize throttle in flight so it can be lifted.
type throttleState struct {
	namespace, pod, container string
	originalCPULimit          string // the limit to put back
	restoreAt                 time.Time
}

// throttle lowers a CPU offender's limit to its request via the /resize
// subresource and schedules its restore. Returns false (→ Event fallback) if the
// container has no CPU limit above its request to lower, or the resize is
// rejected (e.g. in-place resize not enabled on the cluster).
func (r *Remediator) throttle(ctx context.Context, t target, why string) bool {
	pod, err := r.client.CoreV1().Pods(t.namespace).Get(ctx, t.pod, metav1.GetOptions{})
	if err != nil {
		return false
	}

	var limit, request resource.Quantity
	var hasLimit, hasRequest bool
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != t.container {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			limit, hasLimit = q, true
		}
		if q, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			request, hasRequest = q, true
		}
		break
	}
	// Need a request strictly below the current limit, so there is room to
	// throttle down to something the pod is still entitled to.
	if !hasLimit || !hasRequest || request.Cmp(limit) >= 0 {
		return false
	}

	newLimit := request.String()
	origLimit := limit.String()

	if err := r.patchCPULimit(ctx, t.namespace, t.pod, t.container, newLimit); err != nil {
		fmt.Printf("[remediate] resize %s failed (%v) — falling back to Event\n", t.key(), err)
		return false
	}

	restoreAt := r.now().Add(r.cfg.RestoreAfter)
	r.mu.Lock()
	r.throttled[t.key()] = &throttleState{
		namespace: t.namespace, pod: t.pod, container: t.container,
		originalCPULimit: origLimit, restoreAt: restoreAt,
	}
	r.mu.Unlock()

	msg := fmt.Sprintf("node-sentinel: throttled %s CPU limit %s→%s (%s); restoring at %s",
		t.key(), origLimit, newLimit, why, restoreAt.Format("15:04:05"))
	_ = r.emitEvent(ctx, t, msg)
	fmt.Printf("[remediate] %s\n", msg)
	return true
}

// RunRestore lifts elapsed /resize throttles until ctx is cancelled. Start it in
// a goroutine when remediation is enabled.
func (r *Remediator) RunRestore(ctx context.Context) {
	t := time.NewTicker(restoreTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.restoreDue(ctx)
		}
	}
}

// restoreDue restores every throttle whose window has elapsed.
func (r *Remediator) restoreDue(ctx context.Context) {
	now := r.now()
	r.mu.Lock()
	var due []*throttleState
	for k, st := range r.throttled {
		if now.After(st.restoreAt) {
			due = append(due, st)
			delete(r.throttled, k)
		}
	}
	r.mu.Unlock()

	for _, st := range due {
		r.restore(ctx, st)
	}
}

// restore puts a pod's original CPU limit back via /resize and announces it.
func (r *Remediator) restore(ctx context.Context, st *throttleState) {
	key := st.namespace + "/" + st.pod
	if err := r.patchCPULimit(ctx, st.namespace, st.pod, st.container, st.originalCPULimit); err != nil {
		fmt.Printf("[remediate] restore %s failed: %v\n", key, err)
		return
	}
	t := target{namespace: st.namespace, pod: st.pod, container: st.container, resource: "cpu"}
	msg := fmt.Sprintf("node-sentinel: restored %s CPU limit to %s (throttle window elapsed)", key, st.originalCPULimit)
	_ = r.emitEvent(ctx, t, msg)
	fmt.Printf("[remediate] %s\n", msg)
}

// patchCPULimit sets a container's CPU limit through the /resize subresource —
// the kubelet actuates the cgroup change, nothing to fight.
func (r *Remediator) patchCPULimit(ctx context.Context, ns, pod, container, cpu string) error {
	patch := fmt.Sprintf(
		`{"spec":{"containers":[{"name":%q,"resources":{"limits":{"cpu":%q}}}]}}`,
		container, cpu)
	_, err := r.client.CoreV1().Pods(ns).Patch(
		ctx, pod, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}, "resize")
	return err
}
