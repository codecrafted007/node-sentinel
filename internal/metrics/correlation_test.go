package metrics

import (
	"math"
	"testing"
)

// loose config: gate effectively off, so the core math is exercised on its own.
func openCfg(maxLag int) CorrelationConfig {
	return CorrelationConfig{MaxLag: maxLag, MinActive: 1, ActiveFloor: 0}
}

func approx(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func TestCorrelatePerfectPositive(t *testing.T) {
	// victim is exactly 2×offender + 1 — perfectly correlated in shape, scaled
	// and offset. Pearson must report r = 1 (scale/offset invariant).
	off := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	vic := make([]float64, len(off))
	for i, v := range off {
		vic[i] = 2*v + 1
	}
	got := Correlate(off, vic, openCfg(0))
	if got.Gated {
		t.Fatalf("unexpected gate: %+v", got)
	}
	if !approx(got.R, 1, 1e-9) {
		t.Errorf("r = %v, want 1", got.R)
	}
	if got.Lag != 0 {
		t.Errorf("lag = %d, want 0", got.Lag)
	}
}

func TestCorrelateBusyIsNotGuilty(t *testing.T) {
	// The "busy ≠ guilty" guarantee: scaling the offender's magnitude up must
	// NOT change r, because Pearson scores shape, not height. A legitimately
	// loud pod scores the same as a quiet one with the same shape.
	off := []float64{0, 1, 5, 9, 4, 1, 0, 0}
	vic := []float64{0, 0, 2, 4, 2, 1, 0, 0}

	base := Correlate(off, vic, openCfg(2))
	loud := make([]float64, len(off))
	for i, v := range off {
		loud[i] = v * 100 // same shape, 100× the magnitude
	}
	got := Correlate(loud, vic, openCfg(2))

	if !approx(base.R, got.R, 1e-9) {
		t.Errorf("r changed with magnitude: quiet=%v loud=%v (must be equal)", base.R, got.R)
	}
}

func TestCorrelateFindsLag(t *testing.T) {
	// The victim's burst lands one bucket AFTER the offender's. At lag 0 the
	// bursts miss each other; at lag 1 they line up perfectly. The scorer must
	// search lags and report lag 1 with r = 1.
	off := []float64{0, 0, 5, 0, 0, 0, 0, 0}
	vic := []float64{0, 0, 0, 5, 0, 0, 0, 0}

	got := Correlate(off, vic, openCfg(3))
	if got.Gated {
		t.Fatalf("unexpected gate: %+v", got)
	}
	if got.Lag != 1 {
		t.Errorf("lag = %d, want 1", got.Lag)
	}
	if !approx(got.R, 1, 1e-9) {
		t.Errorf("r = %v at lag 1, want 1", got.R)
	}
}

func TestCorrelateAntiCorrelationIsNotGuilt(t *testing.T) {
	// Victim falls as the offender rises — negative correlation. That is not a
	// cause, so Confidence() must clamp it to 0 even though |r| is large.
	off := []float64{1, 2, 3, 4, 5, 6}
	vic := []float64{6, 5, 4, 3, 2, 1}

	got := Correlate(off, vic, openCfg(0))
	if got.R >= 0 {
		t.Fatalf("r = %v, want strongly negative", got.R)
	}
	if c := got.Confidence(); c != 0 {
		t.Errorf("confidence = %v for anti-correlation, want 0", c)
	}
}

func TestCorrelateActivityGate(t *testing.T) {
	// Only one bucket on the victim side is active. With MinActive=4 the gate
	// must block scoring: a high r over near-empty data is exactly the
	// false-positive this guard exists to stop.
	off := []float64{0, 0, 90, 0, 0, 0, 0, 0}
	vic := []float64{0, 0, 88, 0, 0, 0, 0, 0}
	cfg := CorrelationConfig{MaxLag: 2, MinActive: 4, ActiveFloor: 25}

	got := Correlate(off, vic, cfg)
	if !got.Gated {
		t.Errorf("expected gated result, got %+v", got)
	}
	if got.R != 0 || got.Confidence() != 0 {
		t.Errorf("gated result must score 0, got r=%v conf=%v", got.R, got.Confidence())
	}
}

func TestCorrelateVarianceFloorGate(t *testing.T) {
	// The victim is constant (above the active floor, so it passes the count
	// gate) but flat — zero variance. A flat line correlates with nothing
	// meaningful, so the variance floor must block it.
	off := []float64{10, 50, 20, 80, 30, 60}
	vic := []float64{40, 40, 40, 40, 40, 40}
	cfg := CorrelationConfig{MaxLag: 1, MinActive: 1, ActiveFloor: 5, VarianceFloor: 1}

	got := Correlate(off, vic, cfg)
	if !got.Gated {
		t.Errorf("expected variance gate to block flat victim, got %+v", got)
	}
}

func TestCorrelateIndependentSpikeIsLowR(t *testing.T) {
	// The victim suffers — but its spike is elsewhere (bucket 6), unrelated to
	// the offender's burst (bucket 2). Real suffering, wrong offender: r must be
	// low enough that the agent's 0.7 confidence gate rejects it.
	off := []float64{0, 1, 8, 1, 0, 0, 0, 0, 0, 0}
	vic := []float64{0, 0, 0, 0, 0, 0, 7, 1, 0, 0}
	cfg := CorrelationConfig{MaxLag: 3, MinActive: 1, ActiveFloor: 0}

	got := Correlate(off, vic, cfg)
	if got.Confidence() >= 0.7 {
		t.Errorf("independent spike scored %v confidence, want < 0.7", got.Confidence())
	}
}

func TestCorrelateLengthMismatchGated(t *testing.T) {
	got := Correlate([]float64{1, 2, 3}, []float64{1, 2}, openCfg(0))
	if !got.Gated {
		t.Errorf("length mismatch must gate, got %+v", got)
	}
}

func TestPearsonLaggedFlatSeries(t *testing.T) {
	// A flat series has a zero denominator → r defined as 0, not NaN.
	r, n := pearsonLagged([]float64{5, 5, 5, 5}, []float64{1, 2, 3, 4}, 0)
	if r != 0 {
		t.Errorf("flat series r = %v, want 0", r)
	}
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
}

func TestPearsonLaggedNoNaNOnHugeNearConstant(t *testing.T) {
	// A near-constant, high-magnitude series (a steadily-busy pod's CpuNs).
	// The single-pass variance term n·Σx² − (Σx)² is a difference of ~1e31-scale
	// numbers, so float64 cancellation can drive it slightly negative — and the
	// old code did sqrt(negative) = NaN, silently poisoning the offender's
	// confidence. The result must be a valid number, never NaN.
	x := make([]float64, 16)
	for i := range x {
		x[i] = 1e15 + float64(i%2) // ~constant, huge magnitude
	}
	y := []float64{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1}

	r, _ := pearsonLagged(x, y, 0)
	if math.IsNaN(r) {
		t.Fatal("pearsonLagged returned NaN on a huge near-constant series")
	}
	if r < -1 || r > 1 {
		t.Errorf("r = %v, out of [-1, 1]", r)
	}
	// And through the public scorer: Confidence must be a clean 0..1, not NaN.
	res := Correlate(x, y, CorrelationConfig{MaxLag: 0, MinActive: 1, ActiveFloor: 0})
	if c := res.Confidence(); math.IsNaN(c) || c < 0 || c > 1 {
		t.Errorf("Confidence() = %v, want a clean 0..1", c)
	}
}

func TestVariance(t *testing.T) {
	if got := variance([]float64{4, 4, 4}); got != 0 {
		t.Errorf("constant variance = %v, want 0", got)
	}
	// values 2,4,6 → mean 4, population variance = (4+0+4)/3 = 8/3.
	if got, want := variance([]float64{2, 4, 6}), 8.0/3.0; !approx(got, want, 1e-12) {
		t.Errorf("variance = %v, want %v", got, want)
	}
}
