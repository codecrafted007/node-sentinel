package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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
