package metrics

import "sync"

// pruneAfterMisses drops a key's baseline after this many intervals without an
// update (the pod went away).
const pruneAfterMisses = 12

// Baseline learns a per-key "normal" value as an exponential moving average, so
// callers can tell when a key is doing something unusual *for itself* rather
// than relying on one threshold for everything. Think of a fitness watch that
// learns your resting heart rate and notices when yours spikes.
//
// It is safe for concurrent use.
type Baseline struct {
	alpha  float64 // EMA smoothing: higher reacts faster, lower is steadier
	warmup int     // observations before the baseline is trusted

	mu    sync.Mutex
	state map[uint64]*avg
}

type avg struct {
	value   float64
	samples int
	touched bool // observed this round
	misses  int  // consecutive rounds not observed
}

// NewBaseline returns a tracker. alpha in (0,1]; warmup is how many observations
// a key needs before Deviation is trusted.
func NewBaseline(alpha float64, warmup int) *Baseline {
	return &Baseline{alpha: alpha, warmup: warmup, state: map[uint64]*avg{}}
}

// Deviation returns how far current is above the learned normal for key
// (current / baseline) and whether the baseline is established enough to trust.
// It does not modify state.
func (b *Baseline) Deviation(key uint64, current float64) (ratio float64, ready bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	a := b.state[key]
	if a == nil || a.samples < b.warmup || a.value <= 0 {
		return 0, false
	}
	return current / a.value, true
}

// Observe folds current into the baseline for key. When update is false the
// learned value is left unchanged (frozen) — used while a key is flagged as
// anomalous so a sustained spike is not absorbed into "normal" — but the key is
// still kept alive and counts toward warmup.
func (b *Baseline) Observe(key uint64, current float64, update bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	a := b.state[key]
	if a == nil {
		b.state[key] = &avg{value: current, samples: 1, touched: true}
		return
	}
	a.touched = true
	a.misses = 0
	a.samples++
	if update {
		a.value = b.alpha*current + (1-b.alpha)*a.value
	}
}

// Prune drops keys that have not been observed for pruneAfterMisses rounds.
// Call once per interval after observing.
func (b *Baseline) Prune() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for k, a := range b.state {
		if a.touched {
			a.touched = false
			continue
		}
		a.misses++
		if a.misses >= pruneAfterMisses {
			delete(b.state, k)
		}
	}
}

// Len reports how many keys are currently tracked (diagnostics).
func (b *Baseline) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.state)
}
