package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestDurableThrottleSurvivesRestart is the core production-hardening guarantee:
// a throttle persisted by one controller is recovered and restored by a fresh
// controller, so a restart never orphans a throttled pod.
func TestDurableThrottleSurvivesRestart(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	cs := fake.NewSimpleClientset(pod)
	eventNameReactor(cs)
	ctx := context.Background()

	clock := time.Unix(1_000_000, 0)
	mkRem := func() *Remediator {
		r := NewRemediator(cs, RemediationConfig{Resize: true, Cooldown: time.Minute, RestoreAfter: 10 * time.Minute})
		r.now = func() time.Time { return clock }
		r.EnablePersistence("sentinel-system")
		return r
	}

	// --- controller A throttles the offender ---
	a := mkRem()
	a.Remediate(ctx, contendedSnapshot()) // team/hog/main → throttle 2 → 100m
	if got := podCPULimit(cs, "team", "hog"); got != "100m" {
		t.Fatalf("precondition: limit = %s, want throttled 100m", got)
	}
	cm, err := cs.CoreV1().ConfigMaps("sentinel-system").Get(ctx, throttleStateCM, metav1.GetOptions{})
	if err != nil || len(cm.Data) != 1 {
		t.Fatalf("throttle not written to the ledger: err=%v data=%v", err, cm.Data)
	}

	// --- controller B is a fresh process: starts empty, recovers from the ledger ---
	b := mkRem()
	if b.isThrottled("team/hog") {
		t.Fatal("a fresh remediator must start with no in-memory throttles")
	}
	b.Recover(ctx)
	if !b.isThrottled("team/hog") {
		t.Fatal("B did not recover the in-flight throttle from the ledger")
	}

	// --- B restores it on schedule, and clears the ledger ---
	clock = clock.Add(11 * time.Minute)
	b.restoreDue(ctx)
	if got := podCPULimit(cs, "team", "hog"); got != "2" {
		t.Errorf("limit after restore = %s, want the original 2", got)
	}
	if b.isThrottled("team/hog") {
		t.Error("throttle still in memory after restore")
	}
	cm, _ = cs.CoreV1().ConfigMaps("sentinel-system").Get(ctx, throttleStateCM, metav1.GetOptions{})
	if len(cm.Data) != 0 {
		t.Errorf("ledger entry not forgotten after restore: %v", cm.Data)
	}
}

// TestPersistenceDisabledByDefault: without EnablePersistence the throttle is
// in-memory only and Recover is a no-op — the prior behaviour is preserved.
func TestPersistenceDisabledByDefault(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, _ := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute}, pod)
	ctx := context.Background()

	r.Remediate(ctx, contendedSnapshot())
	if !r.isThrottled("team/hog") {
		t.Fatal("expected an in-memory throttle")
	}
	r.Recover(ctx) // no-op, must not panic

	// no ledger ConfigMap should have been created
	if _, err := cs.CoreV1().ConfigMaps("sentinel-system").Get(ctx, throttleStateCM, metav1.GetOptions{}); err == nil {
		t.Error("persistence disabled, but a ledger ConfigMap was created")
	}
}
