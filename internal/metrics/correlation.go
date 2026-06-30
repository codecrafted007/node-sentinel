package metrics

import "math"

// This is the user-space scorer for temporal offender↔victim attribution
// (design v0.2; issue #5). It runs over the per-cgroup sub-interval bucket
// series drained from the kernel (issue #4) and answers one question: do a
// victim's stalls track an offender's bursts *in shape*, not just in size?
//
// The whole thing is pure Go with no Linux dependency — like histogram.go — so
// it is unit-tested on any platform and stays hot-swappable. The kernel only
// ever accumulates integers; all floating-point scoring lives here.
//
// The model mirrors docs/sim/temporal-correlation.html (sim #2): a lagged
// Pearson correlation between the two series, behind a strict activity gate.
// Pearson is the right tool because it is invariant to scale and offset — so a
// legitimately busy pod (large, steady) does not score high just for being
// busy. Only co-movement scores. "Busy ≠ guilty."

// CorrelationConfig tunes the scorer. All fields are in the same unit as the
// series passed to Correlate, so the agent sets them per dimension (run-queue
// latency ns, CPU ns, …) when it wires this up.
type CorrelationConfig struct {
	// MaxLag is the largest offset (in buckets) to search. We try every lag in
	// [0, MaxLag] where the offender *precedes* the victim (cause before effect)
	// and keep the strongest. 0 means "only test coincident buckets".
	MaxLag int
	// MinActive is how many buckets must be "active" on BOTH series before a
	// correlation is trusted. Correlation over a handful of mostly-zero buckets
	// is fragile — a single coincident blip yields a spuriously high r — so
	// below this we refuse to score ("can't tell" beats a guess).
	MinActive int
	// ActiveFloor is the value above which a bucket counts as "active".
	ActiveFloor float64
	// VarianceFloor is the minimum population variance required on BOTH series.
	// It blocks a near-flat line that happens to sit above ActiveFloor from
	// passing the gate (a constant series correlates with nothing meaningful).
	// 0 disables this guard.
	VarianceFloor float64
}

// CorrelationResult is the scorer's verdict for one offender↔victim pair.
type CorrelationResult struct {
	// R is the best lagged Pearson coefficient in [-1, 1]. It is 0 when the gate
	// blocked scoring (see Gated). Positive R means the victim rises as the
	// offender rises; that is the only direction consistent with causation.
	R float64
	// Lag is the offset (in buckets, offender leading) at which R was found.
	Lag int
	// Active is the active-bucket count on the weaker (smaller) side — the
	// number the gate is compared against.
	Active int
	// Gated is true when the activity/variance gate blocked scoring, so R is 0
	// not because the series are uncorrelated but because there wasn't enough
	// signal to judge. Callers should treat this as "not attributable", not "innocent".
	Gated bool
}

// Confidence maps R to a 0–1 attribution confidence, clamping out the negative
// (anti-correlated) half — an offender whose bursts the victim moves *against*
// is not a cause. This is the value the agent feeds into its confidence gate,
// the same way the magnitude/victim signals already do (see offenderConfidence).
func (r CorrelationResult) Confidence() float64 {
	if r.R < 0 {
		return 0
	}
	if r.R > 1 {
		return 1
	}
	return r.R
}

// Correlate scores whether victim tracks offender over the drained sub-interval
// buckets. offender and victim must be the same length and time-aligned (bucket
// i is the same wall-clock window in both); a length mismatch returns a gated,
// zero result rather than guessing.
//
// It returns the strongest positive lagged correlation found, or a gated zero
// when there isn't enough activity to judge honestly.
func Correlate(offender, victim []float64, cfg CorrelationConfig) CorrelationResult {
	if len(offender) != len(victim) || len(offender) == 0 {
		return CorrelationResult{Gated: true}
	}

	// --- the activity gate (anti-false-positive) ---
	offActive := activeCount(offender, cfg.ActiveFloor)
	vicActive := activeCount(victim, cfg.ActiveFloor)
	active := min(offActive, vicActive)
	result := CorrelationResult{Active: active}

	if active < cfg.MinActive {
		result.Gated = true
		return result
	}
	if cfg.VarianceFloor > 0 &&
		(variance(offender) < cfg.VarianceFloor || variance(victim) < cfg.VarianceFloor) {
		result.Gated = true
		return result
	}

	// --- lag search: keep the strongest positive co-movement ---
	// We test only lags where the offender precedes (or is coincident with) the
	// victim, a weak causality guard: a cause cannot follow its effect.
	best := math.Inf(-1)
	bestLag := 0
	for lag := 0; lag <= cfg.MaxLag; lag++ {
		r, n := pearsonLagged(offender, victim, lag)
		if n < 3 {
			break // longer lags only shrink the overlap further
		}
		if r > best {
			best = r
			bestLag = lag
		}
	}
	if math.IsInf(best, -1) {
		result.Gated = true // not enough overlap even at lag 0
		return result
	}

	result.R = best
	result.Lag = bestLag
	return result
}

// pearsonLagged computes the Pearson correlation between x and y with y shifted
// later by lag buckets: it pairs x[i] with y[i+lag]. n is the number of pairs
// that overlapped; r is 0 when either series is (near-)flat over the overlap or
// there are fewer than 3 pairs to correlate.
func pearsonLagged(x, y []float64, lag int) (r float64, n int) {
	count := len(x) - lag
	if count < 3 {
		return 0, count
	}
	var sx, sy, sxy, sxx, syy float64
	for i := range count {
		xi, yi := x[i], y[i+lag] // offender earlier, victim later
		sx += xi
		sy += yi
		sxy += xi * yi
		sxx += xi * xi
		syy += yi * yi
	}
	fn := float64(count)
	num := fn*sxy - sx*sy

	// Each variance term n·Σx² − (Σx)² is a difference of large numbers. For a
	// near-constant high-magnitude series (e.g. a steadily-busy pod's CpuNs),
	// float64 cancellation can drive it slightly negative — and sqrt of a
	// negative is NaN, which would silently poison the offender's confidence. A
	// non-positive term means a (near-)constant series with no shape to
	// correlate, so we report zero correlation.
	vx := fn*sxx - sx*sx
	vy := fn*syy - sy*sy
	if vx <= 0 || vy <= 0 {
		return 0, count
	}
	return num / math.Sqrt(vx*vy), count
}

// activeCount returns how many buckets exceed floor — the "is anything actually
// happening here" measure the gate uses.
func activeCount(xs []float64, floor float64) int {
	n := 0
	for _, x := range xs {
		if x > floor {
			n++
		}
	}
	return n
}

// variance returns the population variance of xs (0 for an empty or constant
// series).
func variance(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		d := x - mean
		ss += d * d
	}
	return ss / float64(len(xs))
}
