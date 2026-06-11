package metrics

import "testing"

func TestBaselineWarmup(t *testing.T) {
	b := NewBaseline(0.5, 3)

	// Not ready until it has `warmup` observations.
	b.Observe(1, 100, true)
	if _, ready := b.Deviation(1, 100); ready {
		t.Fatal("ready after 1 observation, want not ready")
	}
	b.Observe(1, 100, true)
	if _, ready := b.Deviation(1, 100); ready {
		t.Fatal("ready after 2 observations, want not ready")
	}
	b.Observe(1, 100, true)
	if _, ready := b.Deviation(1, 100); !ready {
		t.Fatal("not ready after 3 observations, want ready")
	}
}

func TestBaselineDeviation(t *testing.T) {
	b := NewBaseline(0.5, 1)
	// Settle the baseline near 100.
	for i := 0; i < 5; i++ {
		b.Observe(1, 100, true)
	}
	ratio, ready := b.Deviation(1, 300)
	if !ready {
		t.Fatal("want ready")
	}
	if ratio < 2.9 || ratio > 3.1 {
		t.Fatalf("deviation = %.2f, want ~3.0", ratio)
	}
}

func TestBaselineFreeze(t *testing.T) {
	b := NewBaseline(0.5, 1)
	for i := 0; i < 5; i++ {
		b.Observe(1, 100, true)
	}
	before, _ := b.Deviation(1, 100)
	if before != 1.0 {
		t.Fatalf("baseline ratio for 100 = %.2f, want 1.0", before)
	}
	// A frozen observation must not move the baseline.
	b.Observe(1, 1000, false)
	if ratio, _ := b.Deviation(1, 100); ratio != 1.0 {
		t.Fatalf("baseline moved while frozen: ratio for 100 = %.2f, want 1.0", ratio)
	}
	// An unfrozen observation moves it.
	b.Observe(1, 200, true)
	if ratio, _ := b.Deviation(1, 100); ratio >= 1.0 {
		t.Fatalf("baseline did not rise after update: ratio for 100 = %.2f, want < 1.0", ratio)
	}
}

func TestBaselinePrune(t *testing.T) {
	b := NewBaseline(0.5, 1)
	b.Observe(1, 100, true)
	b.Observe(2, 100, true)
	if b.Len() != 2 {
		t.Fatalf("Len = %d, want 2", b.Len())
	}

	// Keep observing key 1; let key 2 go stale. Needs pruneAfterMisses full
	// unseen rounds after key 2's last observation, plus the round that clears
	// its initial "touched" flag.
	for i := 0; i < pruneAfterMisses+1; i++ {
		b.Observe(1, 100, true)
		b.Prune()
	}
	if _, ok := keyPresent(b, 1); !ok {
		t.Fatal("key 1 pruned but was still observed")
	}
	if _, ok := keyPresent(b, 2); ok {
		t.Fatal("key 2 not pruned after going stale")
	}
}

func keyPresent(b *Baseline, key uint64) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a := b.state[key]
	if a == nil {
		return 0, false
	}
	return a.value, true
}
