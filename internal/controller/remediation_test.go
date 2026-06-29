package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/codecrafted007/node-sentinel/internal/report"
)

func TestSplitPod(t *testing.T) {
	cases := []struct {
		in            string
		ns, pod, c    string
		ok            bool
	}{
		{"payments/api-7f9c/server", "payments", "api-7f9c", "server", true},
		{"system(cg:1234)", "", "", "", false},
		{"unknown", "", "", "", false},
		{"ns/pod", "", "", "", false},
		{"ns//c", "", "", "", false},
	}
	for _, tc := range cases {
		ns, pod, c, ok := splitPod(tc.in)
		if ok != tc.ok || ns != tc.ns || pod != tc.pod || c != tc.c {
			t.Errorf("splitPod(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				tc.in, ns, pod, c, ok, tc.ns, tc.pod, tc.c, tc.ok)
		}
	}
}

func TestConfidentTargetsGate(t *testing.T) {
	s := report.Snapshot{
		ConfidenceMin: 0.7,
		Offenders: []report.Offender{
			{Pod: "ns/hot/app", Confidence: 0.95},  // confident → target
			{Pod: "ns/warm/app", Confidence: 0.40}, // below gate → skip
			{Pod: "system(cg:9)", Confidence: 1.0}, // confident but unattributed → skip
		},
		IOOffenders:  []report.IOOffender{{Pod: "ns/disk/app", Confidence: 0.8}}, // confident → target
		NetOffenders: []report.NetOffender{{Pod: "ns/net/app", Confidence: 0.5}}, // below gate → skip
	}
	got := confidentTargets(s)
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	gotKeys := map[string]string{}
	for _, tg := range got {
		gotKeys[tg.key()] = tg.resource
	}
	if gotKeys["ns/hot"] != "cpu" || gotKeys["ns/disk"] != "disk" {
		t.Errorf("unexpected targets: %v", gotKeys)
	}
}

func newTestRemediator(cfg RemediationConfig, objs ...*corev1.Pod) (*Remediator, *fake.Clientset, *time.Time) {
	cs := fake.NewSimpleClientset()
	// The fake client doesn't honour GenerateName (the apiserver does); simulate
	// it so repeated Event creates get distinct names instead of colliding on "".
	var gen int
	cs.PrependReactor("create", "events", func(a clienttesting.Action) (bool, runtime.Object, error) {
		obj := a.(clienttesting.CreateAction).GetObject()
		if acc, err := meta.Accessor(obj); err == nil && acc.GetName() == "" {
			gen++
			acc.SetName(fmt.Sprintf("%s%d", acc.GetGenerateName(), gen))
		}
		return false, nil, nil // fall through to the default tracker
	})
	for _, p := range objs {
		_, _ = cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
	}
	r := NewRemediator(cs, cfg)
	clock := time.Unix(1_000_000, 0)
	r.now = func() time.Time { return clock }
	return r, cs, &clock
}

func contendedSnapshot() report.Snapshot {
	return report.Snapshot{
		NodeName: "node-a", ConfidenceMin: 0.7,
		Offenders: []report.Offender{{Pod: "team/hog/main", Confidence: 1.0}},
	}
}

func TestRemediateEmitsEvent(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "hog", UID: "uid-123"}}
	r, cs, _ := newTestRemediator(RemediationConfig{Cooldown: time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot())

	evs, _ := cs.CoreV1().Events("team").List(context.Background(), metav1.ListOptions{})
	if len(evs.Items) != 1 {
		t.Fatalf("got %d events, want 1", len(evs.Items))
	}
	e := evs.Items[0]
	if e.Reason != EventReason || e.Type != corev1.EventTypeWarning {
		t.Errorf("event reason/type = %s/%s", e.Reason, e.Type)
	}
	if e.InvolvedObject.Name != "hog" || e.InvolvedObject.UID != "uid-123" {
		t.Errorf("event not anchored to the pod: %+v", e.InvolvedObject)
	}
}

func TestRemediateCooldown(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "hog"}}
	r, cs, clock := newTestRemediator(RemediationConfig{Cooldown: 5 * time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot()) // acts
	r.Remediate(context.Background(), contendedSnapshot()) // within cooldown → skip
	if n := eventCount(cs); n != 1 {
		t.Fatalf("after cooldown window: %d events, want 1", n)
	}

	*clock = clock.Add(6 * time.Minute) // past cooldown
	r.Remediate(context.Background(), contendedSnapshot())
	if n := eventCount(cs); n != 2 {
		t.Errorf("after cooldown elapsed: %d events, want 2", n)
	}
}

func TestRemediateDryRunEmitsNothing(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "hog"}}
	r, cs, _ := newTestRemediator(RemediationConfig{DryRun: true, Cooldown: time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot())
	if n := eventCount(cs); n != 0 {
		t.Errorf("dry-run emitted %d events, want 0", n)
	}
}

func TestRemediateSkipsLowConfidence(t *testing.T) {
	r, cs, _ := newTestRemediator(RemediationConfig{Cooldown: time.Minute})
	s := report.Snapshot{NodeName: "n", ConfidenceMin: 0.7,
		Offenders: []report.Offender{{Pod: "team/quiet/main", Confidence: 0.5}}}
	r.Remediate(context.Background(), s)
	if n := eventCount(cs); n != 0 {
		t.Errorf("low-confidence offender produced %d events, want 0", n)
	}
}

func eventCount(cs *fake.Clientset) int {
	evs, _ := cs.CoreV1().Events("").List(context.Background(), metav1.ListOptions{})
	return len(evs.Items)
}

// --- /resize tier (#7 resize) ---

func cpuPod(ns, name, container, request, limit string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: container,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(request)},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(limit)},
			},
		}}},
	}
}

func podCPULimit(cs *fake.Clientset, ns, name string) string {
	p, _ := cs.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	return p.Spec.Containers[0].Resources.Limits.Cpu().String()
}

func eventContains(cs *fake.Clientset, sub string) bool {
	evs, _ := cs.CoreV1().Events("").List(context.Background(), metav1.ListOptions{})
	for _, e := range evs.Items {
		if strings.Contains(e.Message, sub) {
			return true
		}
	}
	return false
}

func TestResizeThrottlesCPUOffender(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, _ := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute, RestoreAfter: 10 * time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot()) // team/hog/main, cpu, 100%

	if !r.isThrottled("team/hog") {
		t.Fatal("expected the throttle to be recorded")
	}
	if got := podCPULimit(cs, "team", "hog"); got != "100m" {
		t.Errorf("CPU limit after throttle = %s, want 100m (the request)", got)
	}
	if !eventContains(cs, "throttled") {
		t.Error("expected a 'throttled' Event announcing the resize")
	}
}

func TestResizeFallsBackToEventWhenNoRoom(t *testing.T) {
	// request == limit → nothing to throttle down to → Event-tier fallback.
	pod := cpuPod("team", "hog", "main", "100m", "100m")
	r, cs, _ := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot())

	if r.isThrottled("team/hog") {
		t.Error("should not throttle when the limit isn't above the request")
	}
	if !eventContains(cs, "throttling recommended") {
		t.Error("expected the Event-tier fallback")
	}
}

func TestResizeRestoresAfterWindow(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, clock := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute, RestoreAfter: 10 * time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot()) // throttles 2 → 100m
	if got := podCPULimit(cs, "team", "hog"); got != "100m" {
		t.Fatalf("precondition: limit = %s, want 100m", got)
	}

	*clock = clock.Add(11 * time.Minute) // past the restore window
	r.restoreDue(context.Background())

	if r.isThrottled("team/hog") {
		t.Error("throttle should be cleared after restore")
	}
	if got := podCPULimit(cs, "team", "hog"); got != "2" {
		t.Errorf("CPU limit after restore = %s, want 2 (the original)", got)
	}
	if !eventContains(cs, "restored") {
		t.Error("expected a 'restored' Event")
	}
}

func TestResizeNotBeforeRestoreWindow(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, clock := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute, RestoreAfter: 10 * time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot())
	*clock = clock.Add(5 * time.Minute) // still within the window
	r.restoreDue(context.Background())

	if !r.isThrottled("team/hog") {
		t.Error("throttle should still be active before the window elapses")
	}
	if got := podCPULimit(cs, "team", "hog"); got != "100m" {
		t.Errorf("limit = %s, want still-throttled 100m", got)
	}
}

func TestResizeDisabledIsEventOnly(t *testing.T) {
	// Resize:false → CPU offender gets an Event, pod is untouched.
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, _ := newTestRemediator(RemediationConfig{Resize: false, Cooldown: time.Minute}, pod)

	r.Remediate(context.Background(), contendedSnapshot())

	if r.isThrottled("team/hog") {
		t.Error("resize disabled must not throttle")
	}
	if got := podCPULimit(cs, "team", "hog"); got != "2" {
		t.Errorf("limit changed with resize disabled: %s", got)
	}
	if !eventContains(cs, "throttling recommended") {
		t.Error("expected an Event")
	}
}

func TestRemediateNamespaceAllowlist(t *testing.T) {
	pod := cpuPod("team", "hog", "main", "100m", "2")
	r, cs, _ := newTestRemediator(RemediationConfig{Resize: true, Cooldown: time.Minute, Namespaces: []string{"other"}}, pod)

	r.Remediate(context.Background(), contendedSnapshot()) // offender in "team", not allowed

	if r.isThrottled("team/hog") {
		t.Error("must not act on offenders outside the namespace allowlist")
	}
	if eventCount(cs) != 0 {
		t.Errorf("no action expected outside the allowlist, got %d events", eventCount(cs))
	}
}
