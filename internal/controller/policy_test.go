package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestParsePolicy(t *testing.T) {
	obj := map[string]any{
		"metadata": map[string]any{"name": "default"},
		"spec": map[string]any{
			"mode":     "enforce",
			"priority": int64(100),
			"attribution": map[string]any{
				"confidenceThreshold": float64(0.8),
			},
			"remediation": map[string]any{
				"resize":       true,
				"cooldown":     "3m",
				"restoreAfter": "12m",
				"namespaces":   []any{"team-a", "team-b"},
			},
		},
	}
	p := parsePolicy(obj)
	if p.Name != "default" || p.Mode != ModeEnforce || p.Priority != 100 {
		t.Fatalf("meta/mode/priority wrong: %+v", p)
	}
	if p.ConfidenceThreshold != 0.8 {
		t.Errorf("confidenceThreshold = %v, want 0.8", p.ConfidenceThreshold)
	}
	if !p.ResizeSet || !p.Resize {
		t.Errorf("resize parse wrong: set=%v val=%v", p.ResizeSet, p.Resize)
	}
	if p.Cooldown != 3*time.Minute || p.RestoreAfter != 12*time.Minute {
		t.Errorf("durations wrong: %v / %v", p.Cooldown, p.RestoreAfter)
	}
	if len(p.Namespaces) != 2 || p.Namespaces[0] != "team-a" {
		t.Errorf("namespaces wrong: %v", p.Namespaces)
	}
}

func TestParsePolicyDefaultsToObserve(t *testing.T) {
	p := parsePolicy(map[string]any{"metadata": map[string]any{"name": "x"}})
	if p.Mode != ModeObserve {
		t.Errorf("missing spec should default to observe, got %s", p.Mode)
	}
}

func TestPolicyToConfig(t *testing.T) {
	base := NodeHealthPolicy{Cooldown: time.Minute, RestoreAfter: 5 * time.Minute, Namespaces: []string{"ns"}, ConfidenceThreshold: 0.75}

	t.Run("observe is inactive", func(t *testing.T) {
		p := base
		p.Mode = ModeObserve
		if _, active := p.ToConfig(); active {
			t.Error("observe must be inactive")
		}
	})
	t.Run("alert is event-only", func(t *testing.T) {
		p := base
		p.Mode = ModeAlert
		cfg, active := p.ToConfig()
		if !active || cfg.Resize {
			t.Errorf("alert: active=%v resize=%v, want true/false", active, cfg.Resize)
		}
		if cfg.ConfidenceThreshold != 0.75 || cfg.Cooldown != time.Minute {
			t.Errorf("alert config not carried: %+v", cfg)
		}
	})
	t.Run("enforce defaults to resize", func(t *testing.T) {
		p := base
		p.Mode = ModeEnforce // ResizeSet=false
		cfg, active := p.ToConfig()
		if !active || !cfg.Resize {
			t.Errorf("enforce should default to resize: active=%v resize=%v", active, cfg.Resize)
		}
	})
	t.Run("enforce can opt out of resize", func(t *testing.T) {
		p := base
		p.Mode = ModeEnforce
		p.ResizeSet, p.Resize = true, false
		cfg, active := p.ToConfig()
		if !active || cfg.Resize {
			t.Errorf("enforce resize:false → Event-only: active=%v resize=%v", active, cfg.Resize)
		}
	})
}

func nhpObject(name, mode string, priority int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "sentinel.io/v1alpha1",
		"kind":       "NodeHealthPolicy",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"mode": mode, "priority": priority},
	}}
}

func TestLoadActivePolicyPicksHighestPriority(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{nhpGVR: "NodeHealthPolicyList"}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind,
		nhpObject("low", "alert", 10),
		nhpObject("high", "enforce", 100),
		nhpObject("mid", "observe", 50),
	)

	p, ok, err := LoadActivePolicy(context.Background(), dc)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if p.Name != "high" || p.Mode != ModeEnforce {
		t.Errorf("picked %q (%s), want high/enforce", p.Name, p.Mode)
	}
}

func TestLoadActivePolicyNoneFound(t *testing.T) {
	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{nhpGVR: "NodeHealthPolicyList"}
	dc := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)

	if _, ok, err := LoadActivePolicy(context.Background(), dc); ok || err != nil {
		t.Errorf("expected no policy, got ok=%v err=%v", ok, err)
	}
}
