// Durable throttle state (production hardening). The in-place /resize throttle
// and its scheduled restore are tracked in memory; if the controller restarts
// mid-window, an in-memory-only record would be lost and the pod would stay
// throttled forever. To prevent that, active throttles are also written to a
// ConfigMap in the controller's own namespace and reconciled on startup
// (Recover), so a restarted controller resumes and restores them.
//
// A ConfigMap ledger (rather than pod annotations) keeps the pod permissions
// narrow — the controller still only patches pods/resize, never the pod spec —
// and confines the controller's own state to its own namespace. The controller
// is a single replica, so read-modify-write of the ConfigMap is serialised by a
// mutex rather than needing optimistic retries.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// throttleStateCM is the ConfigMap (in the controller's namespace) that records
// active /resize throttles for restart-safety.
const throttleStateCM = "node-sentinel-throttles"

// persistedThrottle is the on-ConfigMap form of a throttleState (one per pod).
type persistedThrottle struct {
	Namespace        string    `json:"namespace"`
	Pod              string    `json:"pod"`
	Container        string    `json:"container"`
	OriginalCPULimit string    `json:"originalCpuLimit"`
	RestoreAt        time.Time `json:"restoreAt"`
}

// EnablePersistence turns on the durable throttle ledger, stored in a ConfigMap
// in ns (the controller's own namespace). With it off (ns == "") throttles are
// in-memory only — the prior behaviour.
func (r *Remediator) EnablePersistence(ns string) { r.stateNS = ns }

// cmKey maps a pod key "ns/pod" to a valid ConfigMap data key (no "/"). Pod and
// namespace names are DNS labels, so they never contain "_", making this unique.
func cmKey(podKey string) string { return strings.ReplaceAll(podKey, "/", "_") }

// persist records a throttle in the ledger. Best-effort: a write failure does not
// block the throttle (it still works for this process), but it is logged because
// it means the throttle would not survive a restart.
func (r *Remediator) persist(ctx context.Context, podKey string, st *throttleState) {
	if r.stateNS == "" {
		return
	}
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	cm, err := r.stateConfigMap(ctx)
	if err != nil {
		fmt.Printf("[remediate] persist %s: %v\n", podKey, err)
		return
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	b, _ := json.Marshal(persistedThrottle{
		Namespace: st.namespace, Pod: st.pod, Container: st.container,
		OriginalCPULimit: st.originalCPULimit, RestoreAt: st.restoreAt,
	})
	cm.Data[cmKey(podKey)] = string(b)
	if _, err := r.client.CoreV1().ConfigMaps(r.stateNS).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		fmt.Printf("[remediate] persist %s: %v\n", podKey, err)
	}
}

// forget removes a throttle from the ledger after it is restored.
func (r *Remediator) forget(ctx context.Context, podKey string) {
	if r.stateNS == "" {
		return
	}
	r.persistMu.Lock()
	defer r.persistMu.Unlock()

	cm, err := r.stateConfigMap(ctx)
	if err != nil {
		return
	}
	if _, ok := cm.Data[cmKey(podKey)]; !ok {
		return
	}
	delete(cm.Data, cmKey(podKey))
	if _, err := r.client.CoreV1().ConfigMaps(r.stateNS).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		fmt.Printf("[remediate] forget %s: %v\n", podKey, err)
	}
}

// Recover loads any throttles persisted by a previous controller into memory, so
// the restore loop will lift them on schedule. Call once at startup before
// RunRestore. A no-op when persistence is disabled.
func (r *Remediator) Recover(ctx context.Context) {
	if r.stateNS == "" {
		return
	}
	cm, err := r.client.CoreV1().ConfigMaps(r.stateNS).Get(ctx, throttleStateCM, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			fmt.Printf("[remediate] recover: %v\n", err)
		}
		return
	}
	n := 0
	r.mu.Lock()
	for _, raw := range cm.Data {
		var p persistedThrottle
		if json.Unmarshal([]byte(raw), &p) != nil || p.Namespace == "" || p.Pod == "" {
			continue
		}
		r.throttled[p.Namespace+"/"+p.Pod] = &throttleState{
			namespace: p.Namespace, pod: p.Pod, container: p.Container,
			originalCPULimit: p.OriginalCPULimit, restoreAt: p.RestoreAt,
		}
		n++
	}
	r.mu.Unlock()
	if n > 0 {
		fmt.Printf("[remediate] recovered %d in-flight throttle(s) from %s/%s\n", n, r.stateNS, throttleStateCM)
	}
}

// stateConfigMap fetches the ledger ConfigMap, creating an empty one if absent.
func (r *Remediator) stateConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	cm, err := r.client.CoreV1().ConfigMaps(r.stateNS).Get(ctx, throttleStateCM, metav1.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	created, err := r.client.CoreV1().ConfigMaps(r.stateNS).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: throttleStateCM, Namespace: r.stateNS},
		Data:       map[string]string{},
	}, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		return r.client.CoreV1().ConfigMaps(r.stateNS).Get(ctx, throttleStateCM, metav1.GetOptions{})
	}
	return created, err
}
