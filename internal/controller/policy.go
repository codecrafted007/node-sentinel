// NodeHealthPolicy support (design §"NodeHealthPolicy"): the controller reads a
// cluster-scoped CR to decide its operating mode and remediation parameters,
// instead of relying solely on flags. The CR is read via the dynamic client (no
// generated clientset — keeps the project's plain-client-go style), and parsing
// + mapping to a RemediationConfig are pure functions so they unit-test without
// a cluster.
//
// This slice acts on the subset the controller can actuate today: mode,
// attribution.confidenceThreshold, and the remediation block. Detection
// thresholds (agent flags), per-node nodeSelector matching, and live re-watch
// are later slices.
package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// nhpGVR is the NodeHealthPolicy custom resource.
var nhpGVR = schema.GroupVersionResource{Group: "sentinel.io", Version: "v1alpha1", Resource: "nodehealthpolicies"}

// PolicyMode is the operating mode (design): observe < alert < enforce.
type PolicyMode string

const (
	ModeObserve PolicyMode = "observe" // aggregate only
	ModeAlert   PolicyMode = "alert"   // emit Events on confident offenders
	ModeEnforce PolicyMode = "enforce" // in-place /resize throttle (else Events)
)

// NodeHealthPolicy is the subset of the CRD this controller acts on.
type NodeHealthPolicy struct {
	Name                string
	Mode                PolicyMode
	Priority            int
	ConfidenceThreshold float64
	Resize              bool
	ResizeSet           bool // whether spec.remediation.resize was given
	Cooldown            time.Duration
	RestoreAfter        time.Duration
	Namespaces          []string
}

// ToConfig maps a policy to a remediation config and whether remediation is
// active. observe → inactive (aggregate only); alert → Event-only; enforce →
// /resize (unless remediation.resize is explicitly false).
func (p NodeHealthPolicy) ToConfig() (RemediationConfig, bool) {
	cfg := RemediationConfig{
		Cooldown:            p.Cooldown,
		RestoreAfter:        p.RestoreAfter,
		Namespaces:          p.Namespaces,
		ConfidenceThreshold: p.ConfidenceThreshold,
	}
	switch p.Mode {
	case ModeAlert:
		cfg.Resize = false
		return cfg, true
	case ModeEnforce:
		cfg.Resize = true // enforce defaults to the /resize tier
		if p.ResizeSet {
			cfg.Resize = p.Resize
		}
		return cfg, true
	default: // observe or unrecognised → do nothing
		return cfg, false
	}
}

// LoadActivePolicy lists NodeHealthPolicies and returns the highest-priority one
// (ok=false if none exist). Ties break by name for determinism.
func LoadActivePolicy(ctx context.Context, dyn dynamic.Interface) (NodeHealthPolicy, bool, error) {
	list, err := dyn.Resource(nhpGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return NodeHealthPolicy{}, false, err
	}
	policies := make([]NodeHealthPolicy, 0, len(list.Items))
	for i := range list.Items {
		policies = append(policies, parsePolicy(list.Items[i].Object))
	}
	if len(policies) == 0 {
		return NodeHealthPolicy{}, false, nil
	}
	sort.SliceStable(policies, func(i, j int) bool {
		if policies[i].Priority != policies[j].Priority {
			return policies[i].Priority > policies[j].Priority
		}
		return policies[i].Name < policies[j].Name
	})
	return policies[0], true, nil
}

// parsePolicy extracts the fields we act on from an unstructured CR object.
// Missing/badly-typed fields fall back to zero values (the schema validates
// shape at admission; this just reads defensively).
func parsePolicy(obj map[string]any) NodeHealthPolicy {
	p := NodeHealthPolicy{Mode: ModeObserve}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		p.Name, _ = meta["name"].(string)
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return p
	}
	if m, ok := spec["mode"].(string); ok && m != "" {
		p.Mode = PolicyMode(m)
	}
	p.Priority = toInt(spec["priority"])
	if attr, ok := spec["attribution"].(map[string]any); ok {
		p.ConfidenceThreshold = toFloat(attr["confidenceThreshold"])
	}
	if rem, ok := spec["remediation"].(map[string]any); ok {
		if v, ok := rem["resize"].(bool); ok {
			p.Resize, p.ResizeSet = v, true
		}
		p.Cooldown = toDuration(rem["cooldown"])
		p.RestoreAfter = toDuration(rem["restoreAfter"])
		p.Namespaces = toStringSlice(rem["namespaces"])
	}
	return p
}

// --- unstructured coercion helpers (JSON numbers arrive as float64/int64) ---

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func toInt(v any) int { return int(toFloat(v)) }

func toDuration(v any) time.Duration {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

func toStringSlice(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Describe renders a one-line summary for logs.
func (p NodeHealthPolicy) Describe() string {
	return fmt.Sprintf("policy %q (priority %d): mode=%s", p.Name, p.Priority, p.Mode)
}
